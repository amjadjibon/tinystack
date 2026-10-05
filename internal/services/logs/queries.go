package logs

import (
	"context"
	"crypto/rand"
	"fmt"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/logs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// A Logs Insights query runs to completion inside StartQuery, so it is
// Complete as soon as it has an ID. Its results are kept under
// <scope>/logs/query/<ID>.

const (
	queryComplete   = "Complete"
	maxQueryGroups  = 50
	maxQueryLength  = 10000
	maxQueryResults = 1000
)

var queryStatuses = []string{"Scheduled", "Running", queryComplete, "Failed", "Cancelled", "Timeout", "Unknown"}

// query is a finished query and its results.
type query struct {
	ID      string
	String  string
	Group   string
	Status  string
	Rows    []row
	Fields  []string
	Created int64
	Matched int
	Scanned int
	Bytes   int
}

func queryKey(scope request.Scope, id string) string { return scope.Prefix("logs", "query") + id }

func newQueryID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// queryGroups resolves the groups StartQuery names, in one of its three ways.
func queryGroups(scope request.Scope, in *api.StartQueryRequest) ([]string, error) {
	given := 0
	for _, set := range []bool{in.LogGroupName != nil, len(in.LogGroupNames) > 0, len(in.LogGroupIdentifiers) > 0} {
		if set {
			given++
		}
	}

	if given != 1 {
		return nil, invalid("Exactly one of logGroupName, logGroupNames, or logGroupIdentifiers must be specified.")
	}

	if in.LogGroupName != nil {
		name, err := resolveGroup(scope, in.LogGroupName, nil)
		return []string{name}, err
	}

	names := make([]string, 0, maxQueryGroups)
	for _, n := range in.LogGroupNames {
		name, err := resolveGroup(scope, &n, nil)
		if err != nil {
			return nil, err
		}

		names = append(names, name)
	}

	for _, id := range in.LogGroupIdentifiers {
		name, err := resolveGroup(scope, nil, &id)
		if err != nil {
			return nil, err
		}

		names = append(names, name)
	}

	if len(names) > maxQueryGroups {
		return nil, invalid("A query may name at most %d log groups.", maxQueryGroups)
	}

	return names, nil
}

// StartQuery runs a query over the events of its groups whose timestamps fall
// in [startTime, endTime], both in seconds, newest first.
func (s *Service) StartQuery(ctx context.Context, in *api.StartQueryRequest) (*api.StartQueryResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	groups, err := queryGroups(scope, in)
	if err != nil {
		return nil, err
	}

	text := common.Value(in.QueryString)
	switch {
	case text == "" || len(text) > maxQueryLength:
		return nil, invalid("queryString must be 1 to %d characters", maxQueryLength)
	case in.StartTime == nil || in.EndTime == nil:
		return nil, invalid("startTime and endTime are required")
	case *in.EndTime < *in.StartTime:
		return nil, invalid("endTime must not be before startTime")
	}

	limit, err := queryLimit(in.Limit)
	if err != nil {
		return nil, err
	}

	commands, err := compileQuery(text)
	if err != nil {
		return nil, err
	}

	q := &query{
		ID: newQueryID(), String: text, Group: groups[0], Status: queryComplete, Created: s.now().UnixMilli(),
	}
	from, to := *in.StartTime*1000, (*in.EndTime+1)*1000
	var rows []row
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, name := range groups {
			g, loadErr := loadGroup(tx, scope, name)
			if loadErr != nil {
				return loadErr
			}

			found, scanErr := scan(tx, scope, g, max(from, g.cutoff(s.now())), to)
			if scanErr != nil {
				return scanErr
			}

			for _, r := range found {
				q.Scanned++
				q.Bytes += len(r["@message"])
			}

			rows = append(rows, found...)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// The fixed-width @timestamp sorts as text.
	slices.SortStableFunc(rows, func(a, b row) int { return strings.Compare(b[fieldTimestamp], a[fieldTimestamp]) })
	q.Rows, q.Fields, q.Matched = run(commands, rows, limit)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, queryKey(scope, q.ID), q)
	})
	if err != nil {
		return nil, err
	}

	return &api.StartQueryResponse{QueryId: new(q.ID)}, nil
}

// scan reads the rows of g's events with timestamps in [from, to).
func scan(tx storage.ReadTx, scope request.Scope, g *group, from, to int64) ([]row, error) {
	var rows []row
	streams := streamPrefix(scope, g.Name)
	for _, key := range tx.Keys(streams) {
		stream := key[len(streams):]
		for _, eventKey := range tx.Keys(eventPrefix(scope, g.Name, stream)) {
			if ts := timestampOf(eventKey); ts < from || ts >= to {
				continue
			}

			e, _, err := storage.Read[event](tx, eventKey)
			if err != nil {
				return nil, err
			}

			rows = append(rows, eventRow(scope.AccountID, g.Name, stream, e))
		}
	}

	return rows, nil
}

func loadQuery(tx storage.ReadTx, scope request.Scope, id *string) (*query, error) {
	q, found, err := storage.Read[query](tx, queryKey(scope, common.Value(id)))
	if err != nil {
		return nil, err
	}

	if !found || common.Value(id) == "" {
		return nil, notFound("The specified query does not exist.")
	}

	return q, nil
}

// GetQueryResults returns a query's rows, each with the fields it has, in the
// query's field order.
func (s *Service) GetQueryResults(
	ctx context.Context,
	in *api.GetQueryResultsRequest,
) (*api.GetQueryResultsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var q *query
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		q, loadErr = loadQuery(tx, scope, in.QueryId)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	out := &api.GetQueryResultsResponse{
		Results: make([][]api.ResultField, 0, len(q.Rows)), Status: new(q.Status),
		Statistics: &api.QueryStatistics{
			RecordsMatched: new(float64(q.Matched)), RecordsScanned: new(float64(q.Scanned)),
			BytesScanned: new(float64(q.Bytes)),
		},
	}
	for _, r := range q.Rows {
		fields := []api.ResultField{}
		for _, f := range q.Fields {
			if v, ok := r[f]; ok {
				fields = append(fields, api.ResultField{Field: new(f), Value: new(v)})
			}
		}

		out.Results = append(out.Results, fields)
	}

	return out, nil
}

// StopQuery stops nothing, since every query has already completed.
func (s *Service) StopQuery(ctx context.Context, in *api.StopQueryRequest) (*api.StopQueryResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, loadErr := loadQuery(tx, scope, in.QueryId)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return &api.StopQueryResponse{Success: new(false)}, nil
}

func (s *Service) DescribeQueries(
	ctx context.Context,
	in *api.DescribeQueriesRequest,
) (*api.DescribeQueriesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit := int64(maxQueryResults)
	switch l := common.Value(in.MaxResults); {
	case in.Status != nil && !slices.Contains(queryStatuses, *in.Status):
		return nil, invalid("status must be one of %s", strings.Join(queryStatuses, ", "))
	case in.MaxResults != nil && (l < 1 || l > maxQueryResults):
		return nil, invalid("maxResults must be from 1 to %d", maxQueryResults)
	case in.MaxResults != nil:
		limit = l
	}

	after, err := decodeToken(in.NextToken)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeQueriesResponse{Queries: []api.QueryInfo{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys(scope.Prefix("logs", "query")) {
			if key <= after {
				continue
			}

			q, _, readErr := storage.Read[query](tx, key)
			if readErr != nil {
				return readErr
			}

			if (in.LogGroupName != nil && q.Group != *in.LogGroupName) ||
				(in.Status != nil && q.Status != *in.Status) {
				continue
			}

			if int64(len(out.Queries)) == limit {
				out.NextToken = encodeToken(after)
				return nil
			}

			out.Queries = append(out.Queries, api.QueryInfo{
				QueryId: new(q.ID), QueryString: new(q.String), Status: new(q.Status), CreateTime: new(q.Created),
				LogGroupName: new(q.Group),
			})
			after = key
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
