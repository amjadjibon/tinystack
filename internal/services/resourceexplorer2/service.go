// Package resourceexplorer2 stores regional indexes and views. Mutations complete
// synchronously: create/update return ACTIVE and delete returns DELETED.
package resourceexplorer2

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/resourceexplorer2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const serviceName = "resourceexplorer2"

type Service struct {
	store  storage.Store
	caller *catalog.Caller
}

func New(store storage.Store) *Service         { return &Service{store: store} }
func (s *Service) UseCaller(c *catalog.Caller) { s.caller = c }

type record struct {
	Index *api.GetIndexOutput `json:"index,omitempty"`
	View  *api.View           `json:"view,omitempty"`
	Tags  map[string]string   `json:"tags,omitempty"`
}

func arn(scope request.Scope, kind, name string) string {
	return fmt.Sprintf("arn:aws:resource-explorer-2:%s:%s:%s/%s", scope.Region, scope.AccountID, kind, name)
}

func missing() error { return awserror.New("ResourceNotFoundException", "Resource was not found.") }
func conflict() error {
	return awserror.New("ConflictException", "Resource already exists or conflicts with an existing resource.")
}

func bad() error {
	return awserror.New("ValidationException", "Invalid query, scope, property, or pagination token.")
}

func size(n *int64) int {
	if n == nil {
		return 1000
	}

	return int(*n)
}

func indexKey(scope request.Scope) string { return scope.Key(serviceName, "index", "default") }
func loadIndex(tx storage.ReadTx, scope request.Scope) (*record, error) {
	r, ok, err := storage.Read[record](tx, indexKey(scope))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing()
	}

	return r, nil
}

func loadView(tx storage.ReadTx, scope request.Scope, a string) (*record, error) {
	r, ok, err := storage.Read[record](tx, scope.Key(serviceName, "view", a))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing()
	}

	return r, nil
}

func (s *Service) CreateIndex(ctx context.Context, in *api.CreateIndexInput) (*api.CreateIndexOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	if err := checkTags(in.Tags); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	a := arn(scope, "index", common.RandomID(32))
	r := record{
		Index: &api.GetIndexOutput{
			Arn:           new(a),
			Type:          new("LOCAL"),
			State:         new("ACTIVE"),
			CreatedAt:     new(now),
			LastUpdatedAt: new(now),
		},
		Tags: common.CloneTags(in.Tags),
	}

	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := idempotency.Key(scope, serviceName, "CreateIndex", in.ClientToken)
		previous, found, replayErr := idempotency.Replay[record](tx, key, in, conflict())
		if replayErr != nil {
			return replayErr
		}

		if found {
			r = previous
			return nil
		}

		_, ok, err := storage.Read[record](tx, indexKey(scope))
		if err != nil {
			return err
		}

		if ok {
			return conflict()
		}

		if writeErr := storage.Write(tx, indexKey(scope), r); writeErr != nil {
			return writeErr
		}

		return idempotency.Remember(tx, key, in, r)
	})
	return &api.CreateIndexOutput{Arn: r.Index.Arn, State: r.Index.State, CreatedAt: r.Index.CreatedAt}, err
}

func (s *Service) GetIndex(ctx context.Context, _ *api.GetIndexInput) (*api.GetIndexOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	var out *api.GetIndexOutput
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		r, err := loadIndex(tx, scope)
		if err != nil {
			return err
		}

		out = r.Index
		out.Tags = r.Tags
		return nil
	})
	return out, err
}

func (s *Service) ListIndexes(ctx context.Context, in *api.ListIndexesInput) (*api.ListIndexesOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	out := &api.ListIndexesOutput{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(scope.AccountID + "/") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok || account != scope.AccountID || rest != "index/default" {
				continue
			}

			r, _, err := storage.Read[record](tx, k)
			if err != nil {
				return err
			}

			if in.Type != nil && *in.Type != common.Value(r.Index.Type) {
				continue
			}

			if len(in.Regions) > 0 && !slices.Contains(in.Regions, region) {
				continue
			}

			out.Indexes = append(out.Indexes, api.Index{Arn: r.Index.Arn, Region: new(region), Type: r.Index.Type})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	out.Indexes, out.NextToken, err = common.Page(
		out.Indexes,
		func(i api.Index) string { return common.Value(i.Region) },
		in.NextToken,
		size(in.MaxResults),
		bad(),
	)
	return out, err
}

func (s *Service) UpdateIndexType(
	ctx context.Context,
	in *api.UpdateIndexTypeInput,
) (*api.UpdateIndexTypeOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	out := &api.UpdateIndexTypeOutput{}
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := loadIndex(tx, scope)
		if err != nil {
			return err
		}

		if common.Value(in.Arn) != common.Value(r.Index.Arn) {
			return missing()
		}

		if common.Value(in.Type) == "AGGREGATOR" {
			for _, k := range tx.Keys(scope.AccountID + "/") {
				_, _, rest, ok := storage.Scoped(k, serviceName)
				if !ok || rest != "index/default" || k == indexKey(scope) {
					continue
				}

				other, _, readErr := storage.Read[record](tx, k)
				if readErr != nil {
					return readErr
				}

				if common.Value(other.Index.Type) == "AGGREGATOR" {
					return conflict()
				}
			}
		}

		r.Index.Type = in.Type
		r.Index.LastUpdatedAt = new(time.Now().UTC())
		*out = api.UpdateIndexTypeOutput{
			Arn:           r.Index.Arn,
			Type:          r.Index.Type,
			State:         r.Index.State,
			LastUpdatedAt: r.Index.LastUpdatedAt,
		}

		return storage.Write(tx, indexKey(scope), r)
	})
	return out, err
}

func (s *Service) DeleteIndex(ctx context.Context, in *api.DeleteIndexInput) (*api.DeleteIndexOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := loadIndex(tx, scope)
		if err != nil {
			return err
		}

		if common.Value(in.Arn) != common.Value(r.Index.Arn) {
			return missing()
		}

		tx.Delete(indexKey(scope))
		for _, key := range tx.Keys(scope.Prefix(serviceName, "view")) {
			tx.Delete(key)
		}

		tx.Delete(scope.Key(serviceName, "default", "view"))
		return nil
	})
	return &api.DeleteIndexOutput{Arn: in.Arn, State: new("DELETED"), LastUpdatedAt: new(time.Now().UTC())}, err
}

var (
	_ api.API           = (*Service)(nil)
	_ catalog.Inventory = (*Service)(nil)
)
