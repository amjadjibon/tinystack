package dynamodb

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	pitrIndex = "aws/dynamodb/index/pitr/"
	hourMS    = int64(time.Hour / time.Millisecond)
	dayMS     = 24 * hourMS
)

type recoveryWindow struct {
	Floor      int64 // earliest available time after enablement or a period reduction
	Days       int64
	LastChange int64
	Sequence   uint64
}

type itemChange struct {
	ID        string
	Previous  json.RawMessage `json:",omitempty"`
	SizeDelta int64
	HadItem   bool
}

func pitrPrefix(key string) string { return key + "/pitr/" }

func pitrHourPrefix(key string, hour int64) string {
	return fmt.Sprintf("%s%013d/", pitrPrefix(key), hour)
}

func pitrIndexKey(key string) string {
	scope, name := tableScope(key)
	return pitrIndex + scope.AccountID + "/" + scope.Region + "/" + name
}

func (o *outbox) recordChange(
	tx storage.WriteTx,
	key string,
	t *table,
	record string,
	old, next item,
	sizeDelta int64,
) error {
	if t.PITR == nil || (old == nil && next == nil) {
		return nil
	}

	if o.changedAt == 0 {
		o.changedAt = o.now().UnixMilli()
	}

	w := t.PITR
	at := max(o.changedAt, w.LastChange, w.Floor)
	w.LastChange = at
	w.Sequence++
	previous, _ := tx.Get(record)
	change := itemChange{
		ID: strings.TrimPrefix(record, itemRecord(key, "")), Previous: previous,
		SizeDelta: sizeDelta, HadItem: next != nil,
	}
	recordKey := fmt.Sprintf("%s%013d/%020d", pitrHourPrefix(key, at/hourMS), at, w.Sequence)
	return storage.Write(tx, recordKey, &change)
}

func (w *recoveryWindow) bounds(now time.Time) (earliest, latest int64) {
	latest = max(now.UnixMilli(), w.LastChange, w.Floor)
	return max(w.Floor, latest-w.Days*dayMS), latest
}

func recoveryTime(t *table, requested *float64, now time.Time, invalidCode string) (float64, error) {
	if t.PITR == nil {
		return 0, awserror.New("PointInTimeRecoveryUnavailableException",
			"Point in time recovery is not enabled for table '"+common.Value(t.Description.TableName)+"'")
	}

	earliest, latest := t.PITR.bounds(now)
	if requested == nil {
		return float64(latest) / 1000, nil
	}

	if math.IsNaN(*requested) || *requested < float64(earliest)/1000 || *requested > float64(latest)/1000 {
		return 0, awserror.New(invalidCode, "The requested time is outside the recoverable time range")
	}

	return *requested, nil
}

type tableSnapshot struct {
	items map[string][]byte
	count int64
	size  int64
}

// snapshot reverses changes strictly after at, starting from the live items.
func snapshot(tx storage.ReadTx, key string, t *table, at float64) (*tableSnapshot, error) {
	prefix := itemRecord(key, "")
	snap := &tableSnapshot{
		items: map[string][]byte{},
		count: common.Value(t.Description.ItemCount),
		size:  common.Value(t.Description.TableSizeBytes),
	}

	for _, record := range itemRecords(tx, key, "") {
		snap.items[strings.TrimPrefix(record, prefix)], _ = tx.Get(record)
	}

	if at >= float64(t.PITR.LastChange)/1000 {
		return snap, nil
	}

	for _, record := range slices.Backward(tx.Keys(pitrPrefix(key))) {
		_, tail, _ := strings.Cut(strings.TrimPrefix(record, pitrPrefix(key)), "/")
		stamp, _, _ := strings.Cut(tail, "/")
		ms, err := strconv.ParseInt(stamp, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("read DynamoDB recovery timestamp: %w", err)
		}

		if float64(ms)/1000 <= at {
			break
		}

		change, _, err := storage.Read[itemChange](tx, record)
		if err != nil {
			return nil, err
		}

		snap.size -= change.SizeDelta
		if change.HadItem {
			snap.count--
		}

		if len(change.Previous) == 0 {
			delete(snap.items, change.ID)
		} else {
			snap.items[change.ID] = change.Previous
			snap.count++
		}
	}

	return snap, nil
}

func clearPITR(tx storage.WriteTx, key string) {
	tx.Delete(pitrIndexKey(key))
	for _, record := range tx.Keys(pitrPrefix(key)) {
		tx.Delete(record)
	}
}

// prunePITR deletes expired hour buckets. A persisted sweep marker keeps each
// table from scanning its full journal until a new bucket can expire.
func (s *Service) prunePITR(ctx context.Context) {
	var keys []string
	_ = s.store.View(ctx, func(tx storage.ReadTx) error {
		keys = tx.Keys(pitrIndex)
		return nil
	})

	for _, index := range keys {
		if ctx.Err() != nil {
			return
		}

		if err := s.prunePITRTable(ctx, index); err != nil && ctx.Err() == nil {
			s.logger.Warn("prune DynamoDB recovery history", "index", index, "error", err)
		}
	}
}

func (s *Service) prunePITRTable(ctx context.Context, index string) error {
	account, rest, _ := strings.Cut(strings.TrimPrefix(index, pitrIndex), "/")
	region, name, _ := strings.Cut(rest, "/")
	key := account + "/" + region + "/dynamodb/" + name
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		last, found, err := storage.Read[int64](tx, index)
		if err != nil || !found {
			return err
		}

		t, err := load(tx, key)
		if err != nil {
			return err
		}

		earliest, _ := t.PITR.bounds(s.now())
		hour := earliest / hourMS
		if hour <= *last {
			return nil
		}

		if hour-*last > 24 {
			// After a long gap, list the journal once instead of looking up
			// every possibly empty bucket.
			before := pitrHourPrefix(key, hour)
			for _, record := range tx.Keys(pitrPrefix(key)) {
				if record >= before {
					break
				}

				tx.Delete(record)
			}
		} else {
			for expiredHour := *last; expiredHour < hour; expiredHour++ {
				for _, record := range tx.Keys(pitrHourPrefix(key, expiredHour)) {
					tx.Delete(record)
				}
			}
		}

		// If the recovery period is later increased, never advertise an
		// interval that has already been dropped.
		t.PITR.Floor = max(t.PITR.Floor, hour*hourMS)
		if saveErr := save(tx, key, t); saveErr != nil {
			return saveErr
		}

		return storage.Write(tx, index, hour)
	})
}
