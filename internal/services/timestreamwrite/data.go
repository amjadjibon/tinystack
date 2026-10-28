package timestreamwrite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamwrite"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// StoredRecord is a canonical time-series point shared with the query engine.
type StoredRecord struct {
	Time         time.Time
	MeasureName  string
	MeasureValue string
	MeasureType  string
	Dimensions   []api.Dimension
	Measures     []api.MeasureValue
	Version      int64
}

func canonical(r *api.Record, at time.Time) *StoredRecord {
	dims := slices.Clone(r.Dimensions)
	for i := range dims {
		dims[i].DimensionValueType = new("VARCHAR")
	}

	slices.SortFunc(
		dims,
		func(a, b api.Dimension) int { return strings.Compare(common.Value(a.Name), common.Value(b.Name)) },
	)
	measures := slices.Clone(r.MeasureValues)
	slices.SortFunc(
		measures,
		func(a, b api.MeasureValue) int { return strings.Compare(common.Value(a.Name), common.Value(b.Name)) },
	)
	version := int64(1)
	if r.Version != nil {
		version = *r.Version
	}

	return &StoredRecord{
		Dimensions: dims, MeasureName: common.Value(r.MeasureName), MeasureValue: common.Value(r.MeasureValue),
		MeasureType: common.Value(r.MeasureValueType), Measures: measures, Time: at.UTC(), Version: version,
	}
}

func equalRecord(a, b *StoredRecord) bool {
	left, right := *a, *b
	left.Version, right.Version = 0, 0
	return reflect.DeepEqual(left, right)
}

func recordPrefix(scope request.Scope, db, table string) string {
	return key(scope, "record", db+"/"+table+"/")
}

func recordKey(scope request.Scope, db, table string, r *StoredRecord) string {
	raw, _ := json.Marshal(struct {
		Measure    string
		Dimensions []api.Dimension
	}{r.MeasureName, r.Dimensions})
	hash := sha256.Sum256(raw)
	series := hex.EncodeToString(hash[:])
	return recordPrefix(scope, db, table) + series + "/" + fmt.Sprintf("%020d", orderedTime(r.Time))
}

// ReadRecords returns points in storage order and removes points beyond magnetic retention at read time.
// Memory retention controls ingestion routing; accepted points migrate logically to magnetic storage.
func (s *Service) ReadRecords(ctx context.Context, scope request.Scope, db, table string) ([]*StoredRecord, error) {
	var out []*StoredRecord
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, found, err := storage.Read[tableRecord](tx, tableKey(scope, db, table))
		if err != nil {
			return err
		}

		if !found {
			return tableNotFound(db, table)
		}

		cutoff := time.Now().AddDate(0, 0, -int(t.MagneticDays))
		for _, k := range tx.Keys(recordPrefix(scope, db, table)) {
			r, _, readErr := storage.Read[StoredRecord](tx, k)
			if readErr != nil {
				return readErr
			}

			if !r.Time.After(cutoff) {
				tx.Delete(k)
				continue
			}

			out = append(out, r)
		}

		return nil
	})
	return out, err
}

// orderedTime flips the sign bit so lexical keys preserve chronological order before and after the epoch.
func orderedTime(at time.Time) uint64 {
	// #nosec G115 -- preserving the two's-complement bits is intentional before flipping the sign bit.
	return uint64(at.UnixNano()) ^ (uint64(1) << 63)
}
