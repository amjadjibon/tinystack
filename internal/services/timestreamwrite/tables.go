package timestreamwrite

import (
	"context"
	"regexp"
	"slices"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamwrite"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	// Defaults of the Timestream guide: six hours in the memory store and a magnetic store that keeps data for 200
	// years, which is its maximum of 73000 days.
	defaultMemoryHours   = 6
	defaultMagneticDays  = 73000
	maxMemoryHours       = 8766
	maxPartitionKeys     = 2
	partitionDimension   = "DIMENSION"
	partitionMeasure     = "MEASURE"
	encryptionSSEKMS     = "SSE_KMS"
	encryptionSSES3      = "SSE_S3"
	enforcementRequired  = "REQUIRED"
	enforcementOptional  = "OPTIONAL"
	bucketNameMinLen     = 3
	bucketNameMaxLen     = 63
	objectKeyPrefixLimit = 928
)

var bucketName = regexp.MustCompile(`^[a-z0-9][.\-a-z0-9]{1,61}[a-z0-9]$`)

type tableRecord struct {
	tagSet
	Created  time.Time
	Updated  time.Time
	Magnetic *api.MagneticStoreWriteProperties
	Schema   *api.Schema
	Database string
	Name     string
	// MemoryHours and MagneticDays are the retention periods.
	MemoryHours  int64
	MagneticDays int64
	// Ingested counts accepted inserts and versioned updates.
	Ingested int64
}

func (r *tableRecord) output(scope request.Scope) *api.Table {
	return &api.Table{
		Arn:          new(tableARN(scope, r.Database, r.Name)),
		TableName:    new(r.Name),
		DatabaseName: new(r.Database),
		TableStatus:  new(statusActive),
		RetentionProperties: &api.RetentionProperties{
			MemoryStoreRetentionPeriodInHours:  new(r.MemoryHours),
			MagneticStoreRetentionPeriodInDays: new(r.MagneticDays),
		},
		CreationTime:                 common.Seconds(r.Created),
		LastUpdatedTime:              common.Seconds(r.Updated),
		MagneticStoreWriteProperties: cloneMagnetic(r.Magnetic),
		Schema:                       cloneSchema(r.Schema),
	}
}

func cloneMagnetic(p *api.MagneticStoreWriteProperties) *api.MagneticStoreWriteProperties {
	if p == nil {
		return nil
	}

	out := *p
	if p.MagneticStoreRejectedDataLocation != nil && p.MagneticStoreRejectedDataLocation.S3Configuration != nil {
		s3 := *p.MagneticStoreRejectedDataLocation.S3Configuration
		out.MagneticStoreRejectedDataLocation = &api.MagneticStoreRejectedDataLocation{S3Configuration: &s3}
	}

	return &out
}

func cloneSchema(s *api.Schema) *api.Schema {
	if s == nil {
		return nil
	}

	return &api.Schema{CompositePartitionKey: slices.Clone(s.CompositePartitionKey)}
}

func checkRetention(p *api.RetentionProperties, r *tableRecord) error {
	if p == nil {
		return nil
	}

	if p.MemoryStoreRetentionPeriodInHours != nil {
		if h := *p.MemoryStoreRetentionPeriodInHours; h < 1 || h > maxMemoryHours {
			return invalid("MemoryStoreRetentionPeriodInHours must be between 1 and %d.", maxMemoryHours)
		}

		r.MemoryHours = *p.MemoryStoreRetentionPeriodInHours
	}

	if p.MagneticStoreRetentionPeriodInDays != nil {
		if d := *p.MagneticStoreRetentionPeriodInDays; d < 1 || d > defaultMagneticDays {
			return invalid("MagneticStoreRetentionPeriodInDays must be between 1 and %d.", defaultMagneticDays)
		}

		r.MagneticDays = *p.MagneticStoreRetentionPeriodInDays
	}

	return nil
}

func checkMagnetic(p *api.MagneticStoreWriteProperties) (*api.MagneticStoreWriteProperties, error) {
	out := cloneMagnetic(p)
	if out.EnableMagneticStoreWrites == nil {
		out.EnableMagneticStoreWrites = new(false)
	}

	if out.MagneticStoreRejectedDataLocation == nil {
		return out, nil
	}

	s3 := out.MagneticStoreRejectedDataLocation.S3Configuration
	if s3 == nil {
		return nil, invalid("MagneticStoreRejectedDataLocation needs an S3Configuration.")
	}

	if n := len(common.Value(s3.BucketName)); n < bucketNameMinLen || n > bucketNameMaxLen ||
		!bucketName.MatchString(*s3.BucketName) {
		return nil, invalid("S3Configuration.BucketName is not a valid bucket name.")
	}

	if s3.ObjectKeyPrefix != nil && (*s3.ObjectKeyPrefix == "" || len(*s3.ObjectKeyPrefix) > objectKeyPrefixLimit) {
		return nil, invalid("S3Configuration.ObjectKeyPrefix must be 1 to %d characters long.", objectKeyPrefixLimit)
	}

	switch common.Value(s3.EncryptionOption) {
	case encryptionSSEKMS:
		if s3.KmsKeyId == nil || *s3.KmsKeyId == "" {
			return nil, invalid("S3Configuration.KmsKeyId is required with %s encryption.", encryptionSSEKMS)
		}
	case encryptionSSES3:
		if s3.KmsKeyId != nil {
			return nil, invalid("S3Configuration.KmsKeyId is only valid with %s encryption.", encryptionSSEKMS)
		}
	case "":
	default:
		return nil, invalid("S3Configuration.EncryptionOption must be %s or %s.", encryptionSSES3, encryptionSSEKMS)
	}

	return out, nil
}

// checkSchema validates a composite partition key: at most two keys, dimension keys named, and a measure key unnamed.
func checkSchema(s *api.Schema) (*api.Schema, error) {
	keys := s.CompositePartitionKey
	if len(keys) < 1 || len(keys) > maxPartitionKeys {
		return nil, invalid("CompositePartitionKey must hold 1 or %d partition keys.", maxPartitionKeys)
	}

	for i, k := range keys {
		switch common.Value(k.Type) {
		case partitionDimension:
			if k.Name == nil || *k.Name == "" {
				return nil, invalid("CompositePartitionKey[%d] is a DIMENSION and needs a Name.", i)
			}
		case partitionMeasure:
			if k.Name != nil {
				return nil, invalid("CompositePartitionKey[%d] is a MEASURE and takes no Name.", i)
			}
		default:
			return nil, invalid(
				"CompositePartitionKey[%d].Type must be %s or %s.", i, partitionDimension, partitionMeasure)
		}

		switch common.Value(k.EnforcementInRecord) {
		case "", enforcementRequired, enforcementOptional:
		default:
			return nil, invalid("CompositePartitionKey[%d].EnforcementInRecord must be %s or %s.", i,
				enforcementRequired, enforcementOptional)
		}
	}

	return cloneSchema(s), nil
}

func (s *Service) CreateTable(ctx context.Context, in *api.CreateTableRequest) (*api.CreateTableResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	db, err := checkName("DatabaseName", in.DatabaseName)
	if err != nil {
		return nil, err
	}

	table, err := checkName("TableName", in.TableName)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	r := &tableRecord{
		Database: db, Name: table, Created: now, Updated: now, MemoryHours: defaultMemoryHours,
		MagneticDays: defaultMagneticDays, tagSet: tagSet{Tags: tags},
	}
	if retentionErr := checkRetention(in.RetentionProperties, r); retentionErr != nil {
		return nil, retentionErr
	}

	if in.MagneticStoreWriteProperties != nil {
		if r.Magnetic, err = checkMagnetic(in.MagneticStoreWriteProperties); err != nil {
			return nil, err
		}
	}

	if in.Schema != nil {
		if r.Schema, err = checkSchema(in.Schema); err != nil {
			return nil, err
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := databaseOf(tx, scope, db); e != nil {
			return e
		}

		if _, exists := tx.Get(tableKey(scope, db, table)); exists {
			return fault("ConflictException", "The table %s already exists in database %s.", table, db)
		}

		return storage.Write(tx, tableKey(scope, db, table), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateTableResponse{Table: r.output(scope)}, nil
}

func (s *Service) DescribeTable(ctx context.Context, in *api.DescribeTableRequest) (*api.DescribeTableResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	db, table, err := tableName(in.DatabaseName, in.TableName)
	if err != nil {
		return nil, err
	}

	var out *api.Table
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if e := databaseOf(tx, scope, db); e != nil {
			return e
		}

		r, found, e := storage.Read[tableRecord](tx, tableKey(scope, db, table))
		if e != nil {
			return e
		}

		if !found {
			return tableNotFound(db, table)
		}

		out = r.output(scope)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeTableResponse{Table: out}, nil
}

func tableName(database, table *string) (db, tbl string, err error) {
	if db, err = checkName("DatabaseName", database); err != nil {
		return "", "", err
	}

	if tbl, err = checkName("TableName", table); err != nil {
		return "", "", err
	}

	return db, tbl, nil
}

func (s *Service) UpdateTable(ctx context.Context, in *api.UpdateTableRequest) (*api.UpdateTableResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	db, table, err := tableName(in.DatabaseName, in.TableName)
	if err != nil {
		return nil, err
	}

	if in.RetentionProperties == nil && in.MagneticStoreWriteProperties == nil && in.Schema == nil {
		return nil, invalid("RetentionProperties, MagneticStoreWriteProperties or Schema is required.")
	}

	var magnetic *api.MagneticStoreWriteProperties
	if in.MagneticStoreWriteProperties != nil {
		if magnetic, err = checkMagnetic(in.MagneticStoreWriteProperties); err != nil {
			return nil, err
		}
	}

	var schema *api.Schema
	if in.Schema != nil {
		if schema, err = checkSchema(in.Schema); err != nil {
			return nil, err
		}
	}

	var out *api.Table
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := databaseOf(tx, scope, db); e != nil {
			return e
		}

		r, found, e := storage.Read[tableRecord](tx, tableKey(scope, db, table))
		if e != nil {
			return e
		}

		if !found {
			return tableNotFound(db, table)
		}

		if retentionErr := checkRetention(in.RetentionProperties, r); retentionErr != nil {
			return retentionErr
		}

		if magnetic != nil {
			r.Magnetic = magnetic
		}

		if schema != nil {
			r.Schema = schema
		}

		r.Updated = time.Now().UTC()
		out = r.output(scope)

		return storage.Write(tx, tableKey(scope, db, table), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateTableResponse{Table: out}, nil
}

func (s *Service) DeleteTable(ctx context.Context, in *api.DeleteTableRequest) (*api.DeleteTableOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	db, table, err := tableName(in.DatabaseName, in.TableName)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := databaseOf(tx, scope, db); e != nil {
			return e
		}

		if _, found := tx.Get(tableKey(scope, db, table)); !found {
			return tableNotFound(db, table)
		}

		for _, k := range tx.Keys(recordPrefix(scope, db, table)) {
			tx.Delete(k)
		}

		tx.Delete(tableKey(scope, db, table))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteTableOutput{}, nil
}

func (s *Service) ListTables(ctx context.Context, in *api.ListTablesRequest) (*api.ListTablesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	prefix := scope.Prefix(serviceName, kindTable)
	if in.DatabaseName != nil {
		db, e := checkName("DatabaseName", in.DatabaseName)
		if e != nil {
			return nil, e
		}

		prefix += db + "/"
	}

	var items []*tableRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if in.DatabaseName != nil {
			if e := databaseOf(tx, scope, *in.DatabaseName); e != nil {
				return e
			}
		}

		var e error
		items, e = storage.Records[tableRecord](tx, prefix)
		return e
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		items, func(r *tableRecord) string { return r.Database + "/" + r.Name }, in.NextToken, limit, invalidToken(),
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListTablesResponse{NextToken: next, Tables: []api.Table{}}
	for _, r := range page {
		out.Tables = append(out.Tables, *r.output(scope))
	}

	return out, nil
}
