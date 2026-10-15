package costexplorer

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/costexplorer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/costdata"
)

const granularityMonthly = "MONTHLY"

func interval(p *api.DateInterval) (start, end time.Time, err error) {
	if p == nil {
		return time.Time{}, time.Time{}, awserror.Validation("TimePeriod is required")
	}

	start, e1 := time.Parse("2006-01-02", common.Value(p.Start))
	end, e2 := time.Parse("2006-01-02", common.Value(p.End))
	if e1 != nil || e2 != nil || !end.After(start) || end.After(start.AddDate(10, 0, 0)) {
		return start, end, awserror.Validation("TimePeriod requires dates, Start < End, and at most ten years")
	}

	return start, end, nil
}

func (s *Service) dataset(
	ctx context.Context,
	p *api.DateInterval,
	filter *api.Expression,
) ([]costdata.Line, []*costdata.Fixture, error) {
	start, end, err := interval(p)
	if err != nil {
		return nil, nil, err
	}

	match, err := costdata.Predicate(filter)
	if err != nil {
		return nil, nil, awserror.Validation("%s", err)
	}

	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	fixtures, err := costdata.Load(ctx, s.store, scope)
	return costdata.Select(costdata.Lines(fixtures), start, end, match), fixtures, err
}

// queryPage binds the cursor to every query parameter, account and fixture revision.
func queryPage[T any](
	items []T,
	query any,
	fixtures []*costdata.Fixture,
	account string,
	token *string,
	size int,
) (page []T, nextToken *string, err error) {
	b, _ := json.Marshal(query)
	hash := sha256.New()
	_, _ = hash.Write(b)
	_, _ = fmt.Fprint(hash, account)
	for _, f := range fixtures {
		_, _ = fmt.Fprint(hash, f.ID, ":", f.Revision, ";")
	}

	digest := hex.EncodeToString(hash.Sum(nil))
	offset := 0
	if token != nil {
		raw, err := base64.RawURLEncoding.DecodeString(*token)
		parts := strings.Split(string(raw), ":")
		if err != nil || len(parts) != 2 || parts[0] != digest {
			return nil, nil, awserror.New("InvalidNextTokenException", "Invalid, changed-query or stale dataset token")
		}

		offset, err = strconv.Atoi(parts[1])
		if err != nil || offset <= 0 || offset >= len(items) || offset%size != 0 {
			return nil, nil, awserror.New("InvalidNextTokenException", "Invalid offset")
		}
	}

	end := min(offset+size, len(items))
	var next *string
	if end < len(items) {
		next = new(base64.RawURLEncoding.EncodeToString([]byte(digest + ":" + strconv.Itoa(end))))
	}

	return items[offset:end], next, nil
}

func metric(lines []costdata.Line) map[string]api.MetricValue {
	return map[string]api.MetricValue{"UnblendedCost": {Amount: new(costdata.Sum(lines)), Unit: new("USD")}}
}

func (s *Service) GetCostAndUsage(
	ctx context.Context,
	in *api.GetCostAndUsageRequest,
) (*api.GetCostAndUsageResponse, error) {
	gran := common.Value(in.Granularity)
	if gran != "DAILY" && gran != granularityMonthly {
		return nil, awserror.Validation("Only DAILY and MONTHLY are supported")
	}

	if len(in.Metrics) != 1 || in.Metrics[0] != "UnblendedCost" {
		return nil, awserror.Validation("Only UnblendedCost is supported")
	}

	if len(in.GroupBy) > 2 {
		return nil, awserror.Validation("At most two grouping keys are supported")
	}

	seen := map[string]bool{}
	for _, g := range in.GroupBy {
		typ, key := common.Value(g.Type), common.Value(g.Key)
		_, dimension := costdata.Dimension(&costdata.Line{}, key)
		if key == "" || typ != "TAG" && (typ != "DIMENSION" || !dimension) || seen[typ+":"+key] {
			return nil, awserror.Validation("Unsupported or duplicate grouping key")
		}

		seen[typ+":"+key] = true
	}

	lines, fixtures, err := s.dataset(ctx, in.TimePeriod, in.Filter)
	if err != nil {
		return nil, err
	}

	start, end, _ := interval(in.TimePeriod)
	var periods []api.ResultByTime
	buckets := costdata.ByPeriod(lines, start, gran == granularityMonthly)
	for cursor := start; cursor.Before(end); {
		next := cursor.AddDate(0, 0, 1)
		if gran == granularityMonthly {
			next = time.Date(cursor.Year(), cursor.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		}

		if next.After(end) {
			next = end
		}

		selected := buckets[cursor.Format("2006-01-02")]
		estimated := false
		for i := range selected {
			l := &selected[i]
			estimated = estimated || l.Estimated
		}

		result := api.ResultByTime{
			TimePeriod: &api.DateInterval{Start: new(cursor.Format("2006-01-02")), End: new(next.Format("2006-01-02"))},
			Estimated:  new(estimated),
		}

		if len(in.GroupBy) == 0 {
			result.Total = metric(selected)
		} else {
			groups := map[string][]costdata.Line{}
			keys := map[string][]string{}
			for i := range selected {
				l := &selected[i]
				var values []string
				for _, g := range in.GroupBy {
					key := common.Value(g.Key)
					value, _ := costdata.Dimension(l, key)
					if common.Value(g.Type) == "TAG" {
						value = key + "$" + l.Tags[key]
					}

					values = append(values, value)
				}

				encoded, _ := json.Marshal(values)
				k := string(encoded)
				groups[k] = append(groups[k], *l)
				keys[k] = values
			}

			ordered := make([]string, 0, len(groups))
			for k := range groups {
				ordered = append(ordered, k)
			}

			slices.Sort(ordered)
			for _, k := range ordered {
				result.Groups = append(result.Groups, api.Group{Keys: keys[k], Metrics: metric(groups[k])})
			}
		}

		periods = append(periods, result)
		cursor = next
	}

	query := *in
	query.NextPageToken = nil
	scope, _ := request.GlobalScope(ctx)
	page, next, err := queryPage(periods, query, fixtures, scope.AccountID, in.NextPageToken, 31)
	return &api.GetCostAndUsageResponse{ResultsByTime: page, NextPageToken: next, GroupDefinitions: in.GroupBy}, err
}

func valuePageSize(n *int64) (int, error) {
	if n == nil {
		return 100, nil
	}

	if *n < 1 || *n > 1000 {
		return 0, awserror.Validation("MaxResults must be 1-1000")
	}

	return int(*n), nil
}

func (s *Service) GetDimensionValues(
	ctx context.Context,
	in *api.GetDimensionValuesRequest,
) (*api.GetDimensionValuesResponse, error) {
	dimension := common.Value(in.Dimension)
	if _, ok := costdata.Dimension(&costdata.Line{}, dimension); !ok {
		return nil, awserror.Validation("Unsupported dimension")
	}

	if in.Context != nil && *in.Context != "COST_AND_USAGE" {
		return nil, awserror.Validation("Only COST_AND_USAGE context is supported")
	}

	size, err := valuePageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	lines, fixtures, err := s.dataset(ctx, in.TimePeriod, in.Filter)
	if err != nil {
		return nil, err
	}

	values := map[string]bool{}
	for i := range lines {
		l := &lines[i]
		v, _ := costdata.Dimension(l, dimension)
		if v != "" && strings.Contains(v, common.Value(in.SearchString)) {
			values[v] = true
		}
	}

	ordered := make([]string, 0, len(values))
	for v := range values {
		ordered = append(ordered, v)
	}

	slices.Sort(ordered)
	query := *in
	query.NextPageToken = nil
	scope, _ := request.GlobalScope(ctx)
	page, next, err := queryPage(ordered, query, fixtures, scope.AccountID, in.NextPageToken, size)
	out := &api.GetDimensionValuesResponse{
		NextPageToken: next,
		ReturnSize:    new(int64(len(page))),
		TotalSize:     new(int64(len(ordered))),
	}

	for _, v := range page {
		out.DimensionValues = append(out.DimensionValues, api.DimensionValuesWithAttributes{Value: new(v)})
	}

	return out, err
}

func (s *Service) GetTags(ctx context.Context, in *api.GetTagsRequest) (*api.GetTagsResponse, error) {
	size, err := valuePageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	if in.TagKey != nil && *in.TagKey == "" {
		return nil, awserror.Validation("TagKey must not be empty")
	}

	lines, fixtures, err := s.dataset(ctx, in.TimePeriod, in.Filter)
	if err != nil {
		return nil, err
	}

	values := map[string]bool{}
	for i := range lines {
		l := &lines[i]
		for k, v := range l.Tags {
			value := k
			if in.TagKey != nil {
				if k != *in.TagKey {
					continue
				}

				value = v
			}

			if strings.Contains(value, common.Value(in.SearchString)) {
				values[value] = true
			}
		}
	}

	ordered := make([]string, 0, len(values))
	for v := range values {
		ordered = append(ordered, v)
	}

	slices.Sort(ordered)
	query := *in
	query.NextPageToken = nil
	scope, _ := request.GlobalScope(ctx)
	page, next, err := queryPage(ordered, query, fixtures, scope.AccountID, in.NextPageToken, size)
	return &api.GetTagsResponse{
		Tags:          page,
		NextPageToken: next,
		ReturnSize:    new(int64(len(page))),
		TotalSize:     new(int64(len(ordered))),
	}, err
}
