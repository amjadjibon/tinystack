package dynamodb

import (
	"context"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	// ttlIndex lists the tables with TTL enabled, as
	// "aws/dynamodb/ttl/<account>/<region>/<table>", so the sweeper reads no
	// other table.
	ttlIndex = "aws/dynamodb/ttl/"
	// ttlHorizon is how far in the past an expiry time may be; DynamoDB ignores
	// older ones.
	ttlHorizon = 5 * 365 * 24 * time.Hour
	// maxExpired bounds how many items one sweep deletes from a table.
	maxExpired = 1000
)

// ttlInterval is how often the sweeper deletes expired items.
var ttlInterval = time.Second

// ttlIndexKey is the TTL index entry of the table stored at key.
func ttlIndexKey(key string) string {
	scope, name := tableScope(key)
	return ttlIndex + scope.AccountID + "/" + scope.Region + "/" + name
}

// DescribeTimeToLive reports whether TTL is enabled and on which attribute.
func (s *Service) DescribeTimeToLive(
	ctx context.Context,
	in *api.DescribeTimeToLiveInput,
) (*api.DescribeTimeToLiveOutput, error) {
	out := &api.DescribeTimeToLiveOutput{
		TimeToLiveDescription: &api.TimeToLiveDescription{TimeToLiveStatus: new("DISABLED")},
	}

	err := s.view(ctx, in.TableName, func(t *table) {
		if t.TTL != "" {
			out.TimeToLiveDescription = &api.TimeToLiveDescription{
				TimeToLiveStatus: new("ENABLED"),
				AttributeName:    new(t.TTL),
			}
		}
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// UpdateTimeToLive enables TTL on an attribute or disables it. The change
// takes effect at once rather than passing through ENABLING or DISABLING.
func (s *Service) UpdateTimeToLive(
	ctx context.Context,
	in *api.UpdateTimeToLiveInput,
) (*api.UpdateTimeToLiveOutput, error) {
	key, err := tableKey(ctx, in.TableName)
	if err != nil {
		return nil, err
	}

	spec := in.TimeToLiveSpecification
	if spec == nil || spec.Enabled == nil {
		return nil, awserror.Validation("TimeToLiveSpecification with Enabled and AttributeName is required")
	}

	if name := common.Value(spec.AttributeName); name == "" || len(name) > 255 {
		return nil, awserror.Validation("TimeToLiveSpecification.AttributeName must be 1 to 255 characters")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		switch {
		case *spec.Enabled && t.TTL != "":
			return awserror.Validation("TimeToLive is already enabled")
		case !*spec.Enabled && t.TTL == "":
			return awserror.Validation("TimeToLive is already disabled")
		case *spec.Enabled:
			t.TTL = *spec.AttributeName
			tx.Put(ttlIndexKey(key), []byte("{}"))
		default:
			t.TTL = ""
			tx.Delete(ttlIndexKey(key))
		}

		return save(tx, key, t)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateTimeToLiveOutput{TimeToLiveSpecification: spec}, nil
}

// expired reports whether a holds an expiry time in epoch seconds that has
// passed, and is not so old that DynamoDB ignores it.
//
//nolint:gocritic // A union is a small fixed set of pointers; it is only read.
func expired(a api.AttributeValue, now time.Time) bool {
	if a.N == nil {
		return false
	}

	r, err := number(*a.N)
	if err != nil {
		return false
	}

	at, _ := r.Float64()
	return at <= float64(now.Unix()) && at >= float64(now.Add(-ttlHorizon).Unix())
}

// sweep deletes expired items every ttlInterval until Stop.
func (s *Service) sweep(ctx context.Context) {
	var tables []string
	_ = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(ttlIndex) {
			scoped := strings.SplitN(strings.TrimPrefix(k, ttlIndex), "/", 3)
			if len(scoped) == 3 {
				tables = append(tables, scoped[0]+"/"+scoped[1]+"/dynamodb/"+scoped[2])
			}
		}

		return nil
	})

	for _, key := range tables {
		if err := s.expire(ctx, key, time.Now()); err != nil && ctx.Err() == nil {
			s.logger.Warn("delete expired DynamoDB items", "table", key, "error", err)
		}
	}
}

// expire deletes the expired items of the table stored at key. Each deletion
// is written to the table's stream as a TTL deletion.
func (s *Service) expire(ctx context.Context, key string, now time.Time) error {
	var records []string
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil || t.TTL == "" {
			return loadErr
		}

		for _, record := range itemRecords(tx, key, "") {
			it, _, itemErr := loadItem(tx, record)
			if itemErr != nil {
				return itemErr
			}

			if expired(it[t.TTL], now) {
				if records = append(records, record); len(records) == maxExpired {
					break
				}
			}
		}

		return nil
	})
	if err != nil || len(records) == 0 {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil || t.TTL == "" {
			return loadErr
		}

		for _, record := range records {
			old, found, itemErr := loadItem(tx, record)
			if itemErr != nil {
				return itemErr
			}

			if !found || !expired(old[t.TTL], now) {
				continue
			}

			if writeErr := writeItemBy(tx, key, t, record, old, nil, ttlIdentity); writeErr != nil {
				return writeErr
			}
		}

		return nil
	})
}
