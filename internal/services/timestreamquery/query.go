package timestreamquery

import (
	"context"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamquery"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type queryRecord struct {
	Result    *api.QueryResponse
	Tokens    map[string]int
	Next      map[int]string
	Limits    map[string]int
	Created   time.Time
	Query     string
	Offset    int
	Uses      int
	Cancelled bool
}

func (s *Service) Query(ctx context.Context, in *api.QueryRequest) (*api.QueryResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit := pageLimit(in.MaxRows, 1000)
	var record *queryRecord
	var id string
	var offset int
	if in.NextToken == nil {
		result, e := s.execute(ctx, common.Value(in.QueryString), time.Now().UTC())
		if e != nil {
			return nil, e
		}

		id = common.Value(result.QueryId)
		record = &queryRecord{
			Query:   common.Value(in.QueryString),
			Created: time.Now(),
			Result:  result,
			Tokens:  map[string]int{},
			Limits:  map[string]int{},
			Next:    map[int]string{},
		}
	} else {
		parts := strings.Split(common.Value(in.NextToken), ":")
		if len(parts) != 2 {
			return nil, invalid("Invalid pagination token")
		}

		id = parts[0]
	}

	var out *api.QueryResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := key(scope, "query", id)
		if record == nil {
			var found bool
			var e error
			record, found, e = storage.Read[queryRecord](tx, k)
			if e != nil {
				return e
			}

			tokenOffset, valid := 0, false
			if found {
				tokenOffset, valid = record.Tokens[common.Value(in.NextToken)]
			}

			offset = tokenOffset
			if valid {
				limit = record.Limits[common.Value(in.NextToken)]
			}

			if !valid || !found || record.Cancelled || time.Since(record.Created) > time.Hour ||
				record.Query != common.Value(in.QueryString) || offset < record.Offset ||
				offset == record.Offset && record.Uses >= 5 || offset < 0 || offset > len(record.Result.Rows) {
				return invalid("Invalid pagination token")
			}

			if offset > record.Offset {
				record.Uses = 0
			}

			record.Offset = offset
			record.Uses++
		}

		out = common.Clone(record.Result)
		end := min(offset+limit, len(out.Rows))
		out.Rows = out.Rows[offset:end]
		if end < len(record.Result.Rows) {
			token := record.Next[end]
			if token == "" {
				token = id + ":" + common.RandomID(32)
				record.Next[end] = token
				record.Tokens[token] = end
				record.Limits[token] = limit
			}

			out.NextToken = new(token)
		} else if in.NextToken == nil {
			// A result that fits one page has no token to read it again.
			return nil
		}

		return storage.Write(tx, k, record)
	})
	return out, err
}

// CancelQuery invalidates unread pages. Queries complete synchronously before their ID is returned.
func (s *Service) CancelQuery(ctx context.Context, in *api.CancelQueryRequest) (*api.CancelQueryResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := key(scope, "query", common.Value(in.QueryId))
		r, found, e := storage.Read[queryRecord](tx, k)
		if e != nil {
			return e
		}

		if !found {
			return invalid("QueryId does not identify a query")
		}

		r.Cancelled = true
		return storage.Write(tx, k, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CancelQueryResponse{CancellationMessage: new("Query has already completed")}, nil
}
