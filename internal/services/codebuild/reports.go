package codebuild

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codebuild"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	reportGroupKind = "report-group"
	reportKind      = "report"
)

type reportRecord struct {
	Report api.Report
	Cases  []api.TestCase
}

func reportMissing() error {
	return awserror.New("ResourceNotFoundException", "Report or report group does not exist")
}

func reportGroupARN(scope request.Scope, name string) string {
	return fmt.Sprintf("arn:aws:codebuild:%s:%s:report-group/%s", scope.Region, scope.AccountID, name)
}

func validateReportGroup(g *api.ReportGroup) error {
	if err := nameOK(g.Name); err != nil {
		return err
	}

	if len(common.Value(g.Name)) > 128 {
		return invalid("Report group name must be 2-128 characters")
	}

	if common.Value(g.Type) != "TEST" {
		return invalid("Only TEST report groups are implemented")
	}

	if g.ExportConfig == nil || common.Value(g.ExportConfig.ExportConfigType) != "NO_EXPORT" {
		return invalid("Only NO_EXPORT report groups are implemented")
	}

	tags, err := tagging.Map(
		g.Tags,
		func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		invalid("Tag key and value are required"),
		func(string) error { return invalid("Duplicate tag key") },
	)
	if err != nil {
		return err
	}

	if err := tagging.CheckSet(tags); err != nil {
		return invalid(err.Error())
	}

	for k, v := range tags {
		if utf8.RuneCountInString(k) > 127 || utf8.RuneCountInString(v) > 255 || !tagPattern.MatchString(k) ||
			!tagPattern.MatchString(v) {
			return invalid("Invalid tag")
		}
	}

	return nil
}

func (s *Service) CreateReportGroup(
	ctx context.Context,
	in *api.CreateReportGroupInput,
) (*api.CreateReportGroupOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	g := &api.ReportGroup{
		Name: in.Name, Type: in.Type, ExportConfig: in.ExportConfig, Tags: in.Tags,
		Created: new(stamp()), LastModified: new(stamp()), Status: new("ACTIVE"),
	}

	if validationErr := validateReportGroup(g); validationErr != nil {
		return nil, validationErr
	}

	g.Arn = new(reportGroupARN(scope, *g.Name))
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, reportGroupKind, *g.Arn)
		if _, found, e := storage.Read[api.ReportGroup](tx, key); e != nil {
			return e
		} else if found {
			return awserror.New("ResourceAlreadyExistsException", "Report group already exists")
		}

		return storage.Write(tx, key, g)
	})
	return &api.CreateReportGroupOutput{ReportGroup: g}, err
}

func (s *Service) UpdateReportGroup(
	ctx context.Context,
	in *api.UpdateReportGroupInput,
) (*api.UpdateReportGroupOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var result *api.ReportGroup
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, reportGroupKind, common.Value(in.Arn))
		g, found, e := storage.Read[api.ReportGroup](tx, key)
		if e != nil {
			return e
		}

		if !found {
			return reportMissing()
		}

		if in.ExportConfig != nil {
			g.ExportConfig = in.ExportConfig
		}

		if in.Tags != nil {
			g.Tags = in.Tags
		}

		if e := validateReportGroup(g); e != nil {
			return e
		}

		g.LastModified = new(stamp())
		result = g
		return storage.Write(tx, key, g)
	})
	return &api.UpdateReportGroupOutput{ReportGroup: result}, err
}

func (s *Service) DeleteReportGroup(
	ctx context.Context,
	in *api.DeleteReportGroupInput,
) (*api.DeleteReportGroupOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, reportGroupKind, common.Value(in.Arn))
		if _, found, e := storage.Read[api.ReportGroup](tx, key); e != nil {
			return e
		} else if !found {
			return reportMissing()
		}

		for _, k := range tx.Keys(scope.Prefix(serviceName, reportKind)) {
			r, _, e := storage.Read[reportRecord](tx, k)
			if e != nil {
				return e
			}

			if common.Value(r.Report.ReportGroupArn) == common.Value(in.Arn) {
				if !common.Value(in.DeleteReports) {
					return invalid("Report group contains reports; set deleteReports")
				}

				tx.Delete(k)
			}
		}

		tx.Delete(key)
		return nil
	})
	return &api.DeleteReportGroupOutput{}, err
}

func (s *Service) BatchGetReportGroups(
	ctx context.Context,
	in *api.BatchGetReportGroupsInput,
) (*api.BatchGetReportGroupsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.ReportGroupArns) < 1 || len(in.ReportGroupArns) > 100 {
		return nil, invalid("Supply 1-100 report group ARNs")
	}

	out := &api.BatchGetReportGroupsOutput{ReportGroups: []api.ReportGroup{}, ReportGroupsNotFound: []string{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		seen := map[string]bool{}
		for _, id := range in.ReportGroupArns {
			if seen[id] {
				continue
			}

			seen[id] = true
			g, found, e := storage.Read[api.ReportGroup](tx, scope.Key(serviceName, reportGroupKind, id))
			if e != nil {
				return e
			}

			if found {
				out.ReportGroups = append(out.ReportGroups, *g)
			} else {
				out.ReportGroupsNotFound = append(out.ReportGroupsNotFound, id)
			}
		}

		return nil
	})
	return out, err
}

func reportPage(ids []string, token *string, limit *int64, order *string) (selected []string, next *string, err error) {
	size := 100
	if limit != nil {
		size = int(*limit)
	}

	if size < 1 || size > 100 {
		return nil, nil, invalid("maxResults must be 1-100")
	}

	if order != nil && *order != ascending && *order != descending {
		return nil, nil, invalid("Invalid sortOrder")
	}

	if common.Value(order) == descending {
		slices.Reverse(ids)
	}

	if token != nil {
		raw, decodeErr := base64.RawURLEncoding.DecodeString(*token)
		if decodeErr != nil || !slices.Contains(ids, string(raw)) {
			return nil, nil, invalid("Invalid nextToken")
		}
	}

	return common.Page(ids, func(id string) string { return id }, token, size, invalid("Invalid nextToken"))
}

func (s *Service) ListReportGroups(
	ctx context.Context,
	in *api.ListReportGroupsInput,
) (*api.ListReportGroupsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in.SortBy != nil && *in.SortBy != "NAME" && *in.SortBy != sortCreated && *in.SortBy != sortModified {
		return nil, invalid("Invalid sortBy")
	}

	var rows []*api.ReportGroup
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		rows, e = storage.Records[api.ReportGroup](tx, scope.Prefix(serviceName, reportGroupKind))
		return e
	})
	if err != nil {
		return nil, err
	}

	slices.SortFunc(rows, func(a, b *api.ReportGroup) int {
		av, bv := cursor{Name: *a.Arn}, cursor{Name: *b.Arn}
		if common.Value(in.SortBy) == sortCreated {
			av.Time = common.Value(a.Created)
			bv.Time = common.Value(b.Created)
		}

		if common.Value(in.SortBy) == sortModified {
			av.Time = common.Value(a.LastModified)
			bv.Time = common.Value(b.LastModified)
		}

		return compare(av, bv)
	})
	ids := []string{}
	for _, g := range rows {
		ids = append(ids, *g.Arn)
	}

	ids, next, err := reportPage(ids, in.NextToken, in.MaxResults, in.SortOrder)
	return &api.ListReportGroupsOutput{ReportGroups: ids, NextToken: next}, err
}

func (s *Service) reportList(
	ctx context.Context,
	group string,
	filter *api.ReportFilter,
	token *string,
	limit *int64,
	order *string,
) (ids []string, next *string, err error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	if filter != nil &&
		!slices.Contains(
			[]string{"GENERATING", statusSucceeded, statusFailed, "INCOMPLETE", "DELETING"},
			common.Value(filter.Status),
		) {
		return nil, nil, invalid("Invalid report status")
	}

	ids = []string{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if group != "" {
			if _, ok, e := storage.Read[api.ReportGroup](tx, scope.Key(serviceName, reportGroupKind, group)); e != nil {
				return e
			} else if !ok {
				return reportMissing()
			}
		}

		rows, e := storage.Records[reportRecord](tx, scope.Prefix(serviceName, reportKind))
		if e != nil {
			return e
		}

		slices.SortFunc(rows, func(a, b *reportRecord) int {
			return compare(
				cursor{Name: common.Value(a.Report.Arn), Time: common.Value(a.Report.Created)},
				cursor{Name: common.Value(b.Report.Arn), Time: common.Value(b.Report.Created)},
			)
		})
		for _, r := range rows {
			if (group == "" || common.Value(r.Report.ReportGroupArn) == group) &&
				(filter == nil || common.Value(r.Report.Status) == common.Value(filter.Status)) {
				ids = append(ids, common.Value(r.Report.Arn))
			}
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return reportPage(ids, token, limit, order)
}

func (s *Service) ListReports(ctx context.Context, in *api.ListReportsInput) (*api.ListReportsOutput, error) {
	ids, next, err := s.reportList(ctx, "", in.Filter, in.NextToken, in.MaxResults, in.SortOrder)
	return &api.ListReportsOutput{Reports: ids, NextToken: next}, err
}

func (s *Service) ListReportsForReportGroup(
	ctx context.Context,
	in *api.ListReportsForReportGroupInput,
) (*api.ListReportsForReportGroupOutput, error) {
	ids, next, err := s.reportList(
		ctx,
		common.Value(in.ReportGroupArn),
		in.Filter,
		in.NextToken,
		in.MaxResults,
		in.SortOrder,
	)
	return &api.ListReportsForReportGroupOutput{Reports: ids, NextToken: next}, err
}

func (s *Service) BatchGetReports(
	ctx context.Context,
	in *api.BatchGetReportsInput,
) (*api.BatchGetReportsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.ReportArns) < 1 || len(in.ReportArns) > 100 {
		return nil, invalid("Supply 1-100 report ARNs")
	}

	out := &api.BatchGetReportsOutput{Reports: []api.Report{}, ReportsNotFound: []string{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		seen := map[string]bool{}
		for _, id := range in.ReportArns {
			if seen[id] {
				continue
			}

			seen[id] = true
			r, ok, e := storage.Read[reportRecord](tx, scope.Key(serviceName, reportKind, id))
			if e != nil {
				return e
			}

			if ok {
				out.Reports = append(out.Reports, r.Report)
			} else {
				out.ReportsNotFound = append(out.ReportsNotFound, id)
			}
		}

		return nil
	})
	return out, err
}

func (s *Service) DescribeTestCases(
	ctx context.Context,
	in *api.DescribeTestCasesInput,
) (*api.DescribeTestCasesOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var cases []api.TestCase
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, ok, e := storage.Read[reportRecord](tx, scope.Key(serviceName, reportKind, common.Value(in.ReportArn)))
		if e != nil {
			return e
		}

		if !ok {
			return reportMissing()
		}

		cases = r.Cases
		return nil
	})
	if err != nil {
		return nil, err
	}

	if in.Filter != nil {
		status := common.Value(in.Filter.Status)
		if status != "" &&
			!slices.Contains([]string{statusSucceeded, statusFailed, "ERROR", "SKIPPED", "UNKNOWN"}, status) {
			return nil, invalid("Invalid test case status")
		}

		cases = slices.DeleteFunc(cases, func(c api.TestCase) bool {
			return status != "" && common.Value(c.Status) != status ||
				(!strings.Contains(common.Value(c.Name), common.Value(in.Filter.Keyword)) &&
					!strings.Contains(common.Value(c.Prefix), common.Value(in.Filter.Keyword)))
		})
	}

	// Stable ordinal keys preserve duplicate test names.
	ids := []string{}
	byID := map[string]api.TestCase{}
	for i, c := range cases {
		status, keyword := "", ""
		if in.Filter != nil {
			status = common.Value(in.Filter.Status)
			keyword = common.Value(in.Filter.Keyword)
		}
		id := fmt.Sprintf("%s/%s/%s/%010d", common.Value(in.ReportArn), status, keyword, i)
		ids = append(ids, id)
		byID[id] = c
	}

	ids, next, err := reportPage(ids, in.NextToken, in.MaxResults, nil)
	out := &api.DescribeTestCasesOutput{TestCases: []api.TestCase{}, NextToken: next}
	for _, id := range ids {
		out.TestCases = append(out.TestCases, byID[id])
	}

	return out, err
}
