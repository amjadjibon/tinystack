package dynamodb

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	capacityIndexes = "INDEXES"
	capacityTotal   = "TOTAL"
	modeNone        = "NONE"
	burstSeconds    = 300
)

type capacityTarget struct {
	key, index string
	global     bool
}

type units struct {
	read, write float64
}

func (u units) capacity() *api.Capacity {
	return &api.Capacity{
		CapacityUnits: new(u.read + u.write), ReadCapacityUnits: new(u.read),
		WriteCapacityUnits: new(u.write),
	}
}

// tally holds the capacity used by a request, including amounts already
// enforced when a PartiQL batch uses multiple independent transactions.
type tally struct {
	spent         map[capacityTarget]units
	accounted     map[capacityTarget]units
	throttled     *capacityTarget
	transactional bool
}

func (t *tally) charge(key string, index *index, bytes int64, consistent bool) {
	n := math.Max(1, math.Ceil(float64(bytes)/readUnitBytes))
	switch {
	case t.transactional:
		n *= 2
	case !consistent:
		n /= 2
	}

	target := capacityTarget{key: key}
	if index != nil {
		target.index, target.global = index.name, index.global
	}

	if t.spent == nil {
		t.spent = map[capacityTarget]units{}
	}

	u := t.spent[target]
	u.read += n
	t.spent[target] = u
}

func (t *tally) chargeWrite(target capacityTarget, bytes int64) {
	n := math.Max(1, math.Ceil(float64(bytes)/writeUnitBytes))
	if t.transactional {
		n *= 2
	}

	if t.spent == nil {
		t.spent = map[capacityTarget]units{}
	}

	u := t.spent[target]
	u.write += n
	t.spent[target] = u
}

func (t *tally) chargeIndexes(key string, table *table, old, next item) error {
	for _, ix := range secondaryIndexes(table) {
		var before, after item
		if ix.holds(old) {
			before = ix.project(table, old)
		}

		if ix.holds(next) {
			after = ix.project(table, next)
		}

		if equal(new(api.AttributeValue{M: &before}), new(api.AttributeValue{M: &after})) {
			continue
		}

		target := capacityTarget{key: key, index: ix.name, global: ix.global}
		sizeBefore, err := itemSize(before)
		if err != nil {
			return err
		}

		sizeAfter, err := itemSize(after)
		if err != nil {
			return err
		}

		if before != nil && after != nil && changedIndexKey(ix, before, after) {
			t.chargeWrite(target, sizeBefore)
			t.chargeWrite(target, sizeAfter)
		} else {
			t.chargeWrite(target, max(sizeBefore, sizeAfter))
		}
	}

	return nil
}

func changedIndexKey(ix *index, before, after item) bool {
	for _, key := range ix.schema {
		a, b := before[*key.AttributeName], after[*key.AttributeName]
		if !equal(&a, &b) {
			return true
		}
	}

	return false
}

func validateCapacity(mode *string) error {
	switch *common.OrElse(mode, new(modeNone)) {
	case modeNone, capacityTotal, capacityIndexes:
		return nil
	default:
		return awserror.Validation("ReturnConsumedCapacity must be INDEXES, TOTAL or NONE")
	}
}

func (t *tally) capacity(mode *string) []api.ConsumedCapacity {
	if mode == nil || *mode == "" || *mode == modeNone {
		return nil
	}

	byTable := map[string]*api.ConsumedCapacity{}
	for target, u := range t.spent {
		c := byTable[target.key]
		if c == nil {
			_, name := tableScope(target.key)
			c = &api.ConsumedCapacity{
				TableName: new(
					name,
				),
				ReadCapacityUnits:  new(0.0),
				WriteCapacityUnits: new(0.0),
				CapacityUnits:      new(0.0),
			}
			byTable[target.key] = c
		}

		*c.ReadCapacityUnits += u.read
		*c.WriteCapacityUnits += u.write
		*c.CapacityUnits += u.read + u.write
		if *mode != capacityIndexes {
			continue
		}

		switch {
		case target.index == "":
			c.Table = u.capacity()
		case target.global:
			if c.GlobalSecondaryIndexes == nil {
				c.GlobalSecondaryIndexes = map[string]api.Capacity{}
			}

			c.GlobalSecondaryIndexes[target.index] = *u.capacity()
		default:
			if c.LocalSecondaryIndexes == nil {
				c.LocalSecondaryIndexes = map[string]api.Capacity{}
			}

			c.LocalSecondaryIndexes[target.index] = *u.capacity()
		}
	}

	var out []api.ConsumedCapacity
	for _, key := range slices.Sorted(maps.Keys(byTable)) {
		out = append(out, *byTable[key])
	}

	return out
}

func (t *tally) singleCapacity(mode *string) *api.ConsumedCapacity {
	out := t.capacity(mode)
	if len(out) == 0 {
		return nil
	}

	return &out[0]
}

// UseProvisionedCapacity turns on enforcement before serving requests.
func (s *Service) UseProvisionedCapacity(enabled bool) { s.enforceCapacity = enabled }

func (s *Service) readData(ctx context.Context, used *tally, run func(storage.ReadTx) error) error {
	if !s.enforceCapacity {
		return s.store.View(ctx, run)
	}

	return s.meteredUpdate(ctx, used, func(tx storage.WriteTx) error { return run(tx) })
}

func (s *Service) meteredUpdate(ctx context.Context, used *tally, run func(storage.WriteTx) error) error {
	if !s.enforceCapacity {
		return s.store.Update(ctx, run)
	}

	before := maps.Clone(used.spent)
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		if err := run(tx); err != nil {
			return err
		}

		return used.enforce(tx, s.now())
	})
	if err != nil {
		used.spent = before
	} else {
		used.accounted = maps.Clone(used.spent)
	}

	return err
}

type tokenBucket struct {
	Updated     int64
	Read, Write float64
}

func capacityPrefix(key string) string { return key + "/capacity/" }

func (t *tally) enforce(tx storage.WriteTx, now time.Time) error {
	pending := map[capacityTarget]units{}
	for target, spent := range t.spent {
		done := t.accounted[target]
		if !target.global {
			target.index = ""
		}

		u := pending[target]
		u.read += spent.read - done.read
		u.write += spent.write - done.write
		pending[target] = u
	}

	for target, cost := range pending {
		if cost == (units{}) {
			continue
		}

		table, err := load(tx, target.key)
		if isCode(err, "ResourceNotFoundException") {
			continue
		}

		if err != nil {
			return err
		}

		if common.Value(common.Value(table.Description.BillingModeSummary).BillingMode) != billingProvisioned {
			continue
		}

		rate := target.capacityRate(table)
		key := capacityPrefix(target.key) + target.index
		bucket, found, err := storage.Read[tokenBucket](tx, key)
		if err != nil {
			return err
		}

		if !found {
			bucket = &tokenBucket{Read: rate.read * burstSeconds, Write: rate.write * burstSeconds}
		}

		at := max(now.UnixMilli(), bucket.Updated)
		elapsed := float64(at-bucket.Updated) / 1000
		rcu := min(rate.read*burstSeconds, bucket.Read+elapsed*rate.read)
		wcu := min(rate.write*burstSeconds, bucket.Write+elapsed*rate.write)
		if cost.read > rcu || cost.write > wcu {
			t.throttled = &target
			return awserror.New("ProvisionedThroughputExceededException",
				"The level of configured provisioned throughput for the table was exceeded")
		}

		if err := storage.Write(tx, key, &tokenBucket{
			Updated: at, Read: rcu - cost.read, Write: wcu - cost.write,
		}); err != nil {
			return err
		}
	}

	return nil
}

func (t capacityTarget) capacityRate(table *table) units {
	desc := table.Description.ProvisionedThroughput
	if t.global {
		for _, ix := range table.Description.GlobalSecondaryIndexes {
			if common.Value(ix.IndexName) == t.index {
				desc = ix.ProvisionedThroughput
				break
			}
		}
	}

	return units{
		read:  float64(common.Value(common.Value(desc).ReadCapacityUnits)),
		write: float64(common.Value(common.Value(desc).WriteCapacityUnits)),
	}
}

type collectionTarget struct {
	key, partition string
}

func (o *outbox) markCollection(key, record string, t *table, it item) {
	if o.collections == nil || len(t.Description.LocalSecondaryIndexes) == 0 || it == nil {
		return
	}

	id := strings.TrimPrefix(record, itemRecord(key, ""))
	partition, _, _ := strings.Cut(id, ":")
	pk := *t.Description.KeySchema[0].AttributeName
	o.collections[collectionTarget{key: key, partition: partition}] = item{pk: it[pk]}
}

func (o *outbox) collectMetrics(tx storage.ReadTx) error {
	for target, key := range o.collections {
		t, err := load(tx, target.key)
		if err != nil {
			return err
		}

		var size int64
		indexes := secondaryIndexes(t)
		for _, record := range itemRecords(tx, target.key, target.partition+":") {
			it, _, err := loadItem(tx, record)
			if err != nil {
				return err
			}

			base, err := itemSize(it)
			if err != nil {
				return err
			}

			size += base
			for _, ix := range indexes {
				if !ix.global && ix.holds(it) {
					indexSize, err := itemSize(ix.project(t, it))
					if err != nil {
						return err
					}

					size += indexSize
				}
			}
		}

		_, name := tableScope(target.key)
		estimate := float64(size) / (1 << 30)
		o.collectionMetrics[name] = append(o.collectionMetrics[name], api.ItemCollectionMetrics{
			ItemCollectionKey: key, SizeEstimateRangeGB: []float64{estimate, estimate},
		})
	}

	return nil
}

func (o *outbox) requestMetrics(mode *string) error {
	switch *common.OrElse(mode, new(modeNone)) {
	case modeNone:
		return nil
	case "SIZE":
		o.collections = map[collectionTarget]item{}
		o.collectionMetrics = map[string][]api.ItemCollectionMetrics{}
		return nil
	default:
		return awserror.Validation("ReturnItemCollectionMetrics must be SIZE or NONE")
	}
}

func (o *outbox) singleCollectionMetric() *api.ItemCollectionMetrics {
	for _, metrics := range o.collectionMetrics {
		return &metrics[0]
	}

	return nil
}

func (s *Service) writeData(ctx context.Context, o *outbox, run func(storage.WriteTx) error) error {
	queued := len(o.puts)
	err := s.meteredUpdate(ctx, &o.used, func(tx storage.WriteTx) error {
		o.changedAt = 0
		if err := run(tx); err != nil {
			return err
		}

		if err := o.collectMetrics(tx); err != nil {
			return fmt.Errorf("collect DynamoDB item collection metrics: %w", err)
		}

		return nil
	})
	if err != nil {
		o.puts = o.puts[:queued]
	}

	return err
}
