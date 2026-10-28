package timestreamwrite

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamwrite"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxRecords       = 100
	maxDimensions    = 128
	maxDimensionName = 60
	maxDimensionVal  = 2048
	maxMeasureName   = 256

	// A record may be at most this far ahead of the clock; the Timestream guide gives the limit as 15 minutes.
	futureWindow = 15 * time.Minute

	measureMulti = "MULTI"
)

// A record counts toward the memory store when its time is inside the memory retention period, and toward the
// magnetic store when magnetic store writes are enabled and the time is inside the magnetic retention period.
type destination int

const (
	toMemory destination = iota
	toMagnetic
	rejected
)

// merge applies the table-wide common attributes under the record's own.
func merge(shared, in *api.Record) api.Record {
	r := *in
	if shared == nil {
		if r.MeasureValueType == nil {
			r.MeasureValueType = new("DOUBLE")
		}

		return r
	}

	if r.Dimensions == nil {
		r.Dimensions = shared.Dimensions
	} else if len(shared.Dimensions) > 0 {
		r.Dimensions = append(append([]api.Dimension{}, shared.Dimensions...), r.Dimensions...)
	}

	r.MeasureName = common.OrElse(r.MeasureName, shared.MeasureName)
	r.MeasureValue = common.OrElse(r.MeasureValue, shared.MeasureValue)
	r.MeasureValueType = common.OrElse(r.MeasureValueType, shared.MeasureValueType)
	if r.MeasureValueType == nil {
		r.MeasureValueType = new("DOUBLE")
	}

	r.Time = common.OrElse(r.Time, shared.Time)
	r.TimeUnit = common.OrElse(r.TimeUnit, shared.TimeUnit)

	if r.Version == nil {
		r.Version = shared.Version
	}

	if r.MeasureValues == nil {
		r.MeasureValues = shared.MeasureValues
	}

	return r
}

// checkMeasure validates a measure value against its type.
func checkMeasure(kind, value string) error {
	var err error
	switch kind {
	case "DOUBLE":
		_, err = strconv.ParseFloat(value, 64)
	case "BIGINT", "TIMESTAMP":
		_, err = strconv.ParseInt(value, 10, 64)
	case "BOOLEAN":
		if v := strings.ToLower(value); v != "true" && v != "false" {
			err = strconv.ErrSyntax
		}
	case "VARCHAR":
		if value == "" || len(value) > 2048 {
			err = strconv.ErrRange
		}
	default:
		return invalid("MeasureValueType %q is not supported.", kind)
	}

	if err != nil {
		return invalid("The measure value %q is not a valid %s.", value, kind)
	}

	return nil
}

// validate checks the structure of one merged record, failing the whole request on a malformed one.
func validate(r *api.Record) error {
	if len(r.Dimensions) > maxDimensions {
		return invalid("A record can have at most %d dimensions.", maxDimensions)
	}

	names := map[string]bool{"time": true, "measure_name": true}

	for _, d := range r.Dimensions {
		dimension := common.Value(d.Name)
		if names[dimension] {
			return invalid("Duplicate or reserved dimension name: %s", dimension)
		}

		names[dimension] = true
		if n := len(common.Value(d.Name)); n < 1 || n > maxDimensionName {
			return invalid("A dimension name must be 1 to %d bytes long.", maxDimensionName)
		}

		if n := len(common.Value(d.Value)); n < 1 || n > maxDimensionVal {
			return invalid("The value of dimension %s must be 1 to %d bytes long.", *d.Name, maxDimensionVal)
		}

		if d.DimensionValueType != nil && *d.DimensionValueType != "VARCHAR" {
			return invalid("DimensionValueType must be VARCHAR.")
		}
	}

	if n := len(common.Value(r.MeasureName)); n < 1 || n > maxMeasureName {
		return invalid("MeasureName must be 1 to %d characters long.", maxMeasureName)
	}

	kind := common.Value(r.MeasureValueType)
	if kind == measureMulti {
		if r.MeasureValue != nil || len(r.MeasureValues) == 0 {
			return invalid("A MULTI record takes MeasureValues and no MeasureValue.")
		}

		for _, m := range r.MeasureValues {
			measure := common.Value(m.Name)
			if names[measure] {
				return invalid("Duplicate or reserved measure column: %s", measure)
			}

			names[measure] = true
			if common.Value(m.Name) == "" || common.Value(m.Type) == measureMulti {
				return invalid("Every measure value needs a Name and a Type other than MULTI.")
			}

			if err := checkMeasure(common.Value(m.Type), common.Value(m.Value)); err != nil {
				return err
			}
		}
	} else {
		if r.MeasureValue == nil || len(r.MeasureValues) > 0 {
			return invalid("A %s record takes a MeasureValue and no MeasureValues.", kind)
		}

		if err := checkMeasure(kind, *r.MeasureValue); err != nil {
			return err
		}
	}

	if r.Version != nil && *r.Version < 1 {
		return invalid("Version must be positive.")
	}

	return nil
}

// instant reads a record's Time in its TimeUnit.
func instant(r *api.Record) (time.Time, error) {
	if r.Time == nil {
		return time.Time{}, invalid("Time is required.")
	}

	n, err := strconv.ParseInt(*r.Time, 10, 64)
	if err != nil {
		return time.Time{}, invalid("Time %q is not an integer.", *r.Time)
	}

	switch common.Value(r.TimeUnit) {
	case "SECONDS":
		return time.Unix(n, 0), nil
	case "", "MILLISECONDS":
		return time.UnixMilli(n), nil
	case "MICROSECONDS":
		return time.UnixMicro(n), nil
	case "NANOSECONDS":
		return time.Unix(0, n), nil
	default:
		return time.Time{}, invalid("TimeUnit %q is not supported.", *r.TimeUnit)
	}
}

// route decides where a record's time falls, and why it falls nowhere.
func route(t *tableRecord, at, now time.Time) (where destination, reason string) {
	switch {
	case at.After(now.Add(futureWindow)):
		return rejected, "The record timestamp is newer than the allowed ingestion window."
	case at.After(now.Add(-time.Duration(t.MemoryHours) * time.Hour)):
		return toMemory, ""
	case t.Magnetic != nil && common.Value(t.Magnetic.EnableMagneticStoreWrites) &&
		at.After(now.AddDate(0, 0, -int(t.MagneticDays))):
		return toMagnetic, ""
	default:
		return rejected, "The record timestamp is outside the time range of the data ingestion window."
	}
}

// atRecord prefixes a validation failure with the index of the record it concerns.
func atRecord(i int, err error) error {
	var e *awserror.Error
	if errors.As(err, &e) {
		return awserror.Validation("Record %d: %s", i, e.Message)
	}

	return err
}

// WriteRecords validates and persists records, committing accepted records even when others are rejected.
func (s *Service) WriteRecords(ctx context.Context, in *api.WriteRecordsRequest) (*api.WriteRecordsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	db, table, err := tableName(in.DatabaseName, in.TableName)
	if err != nil {
		return nil, err
	}

	if len(in.Records) < 1 || len(in.Records) > maxRecords {
		return nil, invalid("Records must hold 1 to %d records.", maxRecords)
	}

	merged := make([]api.Record, len(in.Records))
	times := make([]time.Time, len(in.Records))
	for i := range in.Records {
		merged[i] = merge(in.CommonAttributes, &in.Records[i])
		if err = validate(&merged[i]); err != nil {
			return nil, atRecord(i, err)
		}

		if times[i], err = instant(&merged[i]); err != nil {
			return nil, atRecord(i, err)
		}
	}

	var memory, magnetic int64
	var reasons []any
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := databaseOf(tx, scope, db); e != nil {
			return e
		}

		t, found, e := storage.Read[tableRecord](tx, tableKey(scope, db, table))
		if e != nil {
			return e
		}

		if !found {
			return tableNotFound(db, table)
		}

		now := time.Now()
		for i, at := range times {
			where, reason := route(t, at, now)
			if where == rejected {
				reasons = append(reasons, map[string]any{"RecordIndex": i, "Reason": reason})
				continue
			}

			r := canonical(&merged[i], at)
			k := recordKey(scope, db, table, r)
			old, exists, readErr := storage.Read[StoredRecord](tx, k)
			if readErr != nil {
				return readErr
			}

			if exists && r.Version <= old.Version {
				if equalRecord(r, old) {
					continue
				}

				reasons = append(reasons, map[string]any{
					"RecordIndex":     i,
					"Reason":          "The record version must be greater than the existing record version.",
					"ExistingVersion": old.Version,
				})
				continue
			}

			if writeErr := storage.Write(tx, k, r); writeErr != nil {
				return writeErr
			}

			if where == toMemory {
				memory++
			} else {
				magnetic++
			}
		}

		t.Ingested += memory + magnetic

		return storage.Write(tx, tableKey(scope, db, table), t)
	})
	if err != nil {
		return nil, err
	}

	if len(reasons) > 0 {
		out := awserror.New("RejectedRecordsException",
			"One or more records have been rejected. See RejectedRecords in the response for details.")
		out.StatusCode = 400
		out.Fields = map[string]any{"RejectedRecords": reasons}

		return nil, out
	}

	return &api.WriteRecordsResponse{
		RecordsIngested: &api.RecordsIngested{
			Total: new(memory + magnetic), MemoryStore: new(memory), MagneticStore: new(magnetic),
		},
	}, nil
}
