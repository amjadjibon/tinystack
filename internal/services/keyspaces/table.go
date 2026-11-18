package keyspaces

import (
	"context"
	"regexp"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/keyspaces"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var kmsKeyARN = regexp.MustCompile(`^arn:aws(?:-[a-z0-9-]+)?:kms:[a-z0-9-]+:[0-9]{12}:key/[A-Za-z0-9-]+$`)

const onDemand = "PAY_PER_REQUEST"

func (s *Service) CreateTable(ctx context.Context, in *api.CreateTableRequest) (*api.CreateTableResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	ks, name := common.Value(in.KeyspaceName), common.Value(in.TableName)
	if writableErr := writable(ks); writableErr != nil {
		return nil, writableErr
	}

	tags, err := checkTags(in.Tags)
	if err != nil {
		return nil, err
	}

	arn := resourceARN(scope, ks, name)
	now := time.Now().UTC()
	table := &api.GetTableResponse{
		KeyspaceName: in.KeyspaceName, TableName: in.TableName, ResourceArn: new(arn),
		CreationTimestamp: common.Seconds(now), SchemaDefinition: in.SchemaDefinition, Comment: in.Comment,
		CapacitySpecification:   &api.CapacitySpecificationSummary{ThroughputMode: new(onDemand)},
		EncryptionSpecification: &api.EncryptionSpecification{Type: new("AWS_OWNED_KMS_KEY")},
		PointInTimeRecovery:     &api.PointInTimeRecoverySummary{Status: new("DISABLED")},
		DefaultTimeToLive:       new(int64(0)),
	}
	options := tableOptions{
		capacity: in.CapacitySpecification, encryption: in.EncryptionSpecification, pitr: in.PointInTimeRecovery,
		ttl: in.Ttl, defaultTTL: in.DefaultTimeToLive, timestamps: in.ClientSideTimestamps,
	}
	if optionErr := options.apply(table, now); optionErr != nil {
		return nil, optionErr
	}

	key := childKey(scope, kindTable, ks, name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := requireKeyspace(tx, scope, ks); e != nil {
			return e
		}

		if e := createRecord(tx, key); e != nil {
			return e
		}

		defined, e := definedTypes(tx, scope, ks)
		if e != nil {
			return e
		}

		types, refs, e := schemaTypes(table.SchemaDefinition, defined)
		if e != nil {
			return e
		}

		if e := checkCounterTTL(table, types); e != nil {
			return e
		}

		r := &record{Keyspace: ks, Name: name, Created: now, Table: table, Tags: tags, TypeRefs: refs}
		return s.writeChange(tx, scope, key, r, creating, createTableCQL(table, types))
	})
	return &api.CreateTableResponse{ResourceArn: new(arn)}, err
}

func (s *Service) UpdateTable(ctx context.Context, in *api.UpdateTableRequest) (*api.UpdateTableResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	ks, name := common.Value(in.KeyspaceName), common.Value(in.TableName)
	if writableErr := writable(ks); writableErr != nil {
		return nil, writableErr
	}

	options := tableOptions{
		capacity: in.CapacitySpecification, encryption: in.EncryptionSpecification, pitr: in.PointInTimeRecovery,
		ttl: in.Ttl, defaultTTL: in.DefaultTimeToLive, timestamps: in.ClientSideTimestamps,
	}
	if len(in.AddColumns) == 0 && options == (tableOptions{}) {
		return nil, awserror.Validation("UpdateTable requires at least one change")
	}

	key := childKey(scope, kindTable, ks, name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readRecord(tx, key)
		if e != nil {
			return e
		}

		if optionErr := options.apply(r.Table, time.Now().UTC()); optionErr != nil {
			return optionErr
		}

		defined, e := definedTypes(tx, scope, ks)
		if e != nil {
			return e
		}

		schema := r.Table.SchemaDefinition
		schema.AllColumns = append(schema.AllColumns, in.AddColumns...)
		types, refs, e := schemaTypes(schema, defined)
		if e != nil {
			return e
		}

		if e := checkCounterTTL(r.Table, types); e != nil {
			return e
		}

		r.TypeRefs = refs
		return s.writeChange(tx, scope, key, r, updating, alterTableCQL(in, types)...)
	})
	return &api.UpdateTableResponse{ResourceArn: new(resourceARN(scope, ks, name))}, err
}

type tableOptions struct {
	capacity   *api.CapacitySpecification
	encryption *api.EncryptionSpecification
	pitr       *api.PointInTimeRecovery
	ttl        *api.TimeToLive
	defaultTTL *int64
	timestamps *api.ClientSideTimestamps
}

func (o tableOptions) apply(table *api.GetTableResponse, now time.Time) error {
	if o.capacity != nil {
		if err := applyCapacity(table, o.capacity, now); err != nil {
			return err
		}
	}

	if o.encryption != nil {
		kind, key := common.Value(o.encryption.Type), o.encryption.KmsKeyIdentifier
		switch kind {
		case "AWS_OWNED_KMS_KEY":
			if key != nil {
				return awserror.Validation("KmsKeyIdentifier is only valid for CUSTOMER_MANAGED_KMS_KEY")
			}
		case "CUSTOMER_MANAGED_KMS_KEY":
			if !kmsKeyARN.MatchString(common.Value(key)) {
				return awserror.Validation("CUSTOMER_MANAGED_KMS_KEY requires a KMS key ARN")
			}
		default:
			return awserror.Validation("Invalid encryption type %q", kind)
		}

		table.EncryptionSpecification = o.encryption
	}

	if o.pitr != nil {
		status := common.Value(o.pitr.Status)
		switch status {
		case enabled:
			if common.Value(table.PointInTimeRecovery.Status) != status {
				table.PointInTimeRecovery = &api.PointInTimeRecoverySummary{
					Status: o.pitr.Status, EarliestRestorableTimestamp: common.Seconds(now),
				}
			}
		case "DISABLED":
			table.PointInTimeRecovery = &api.PointInTimeRecoverySummary{Status: o.pitr.Status}
		default:
			return awserror.Validation("Point-in-time recovery status must be ENABLED or DISABLED")
		}
	}

	if o.ttl != nil {
		if common.Value(o.ttl.Status) != enabled {
			return awserror.Validation("TTL status must be ENABLED")
		}

		table.Ttl = o.ttl
	}

	if o.defaultTTL != nil {
		if *o.defaultTTL < 0 || *o.defaultTTL > 630720000 {
			return awserror.Validation("DefaultTimeToLive must be between 0 and 630720000")
		}

		table.DefaultTimeToLive = o.defaultTTL
		if *o.defaultTTL > 0 {
			table.Ttl = &api.TimeToLive{Status: new(enabled)}
		}
	}

	if o.timestamps != nil {
		if common.Value(o.timestamps.Status) != enabled {
			return awserror.Validation("Client-side timestamps status must be ENABLED")
		}

		table.ClientSideTimestamps = o.timestamps
	}

	return nil
}

func applyCapacity(table *api.GetTableResponse, spec *api.CapacitySpecification, now time.Time) error {
	previous := table.CapacitySpecification
	next := &api.CapacitySpecificationSummary{
		ThroughputMode:                     spec.ThroughputMode,
		LastUpdateToPayPerRequestTimestamp: previous.LastUpdateToPayPerRequestTimestamp,
	}
	switch common.Value(spec.ThroughputMode) {
	case onDemand:
		if spec.ReadCapacityUnits != nil || spec.WriteCapacityUnits != nil {
			return awserror.Validation("Capacity units are only valid for PROVISIONED throughput")
		}

		if common.Value(previous.ThroughputMode) != onDemand {
			next.LastUpdateToPayPerRequestTimestamp = common.Seconds(now)
		}
	case "PROVISIONED":
		if common.Value(spec.ReadCapacityUnits) < 1 || common.Value(spec.WriteCapacityUnits) < 1 {
			return awserror.Validation("PROVISIONED throughput requires positive read and write capacity units")
		}

		next.ReadCapacityUnits, next.WriteCapacityUnits = spec.ReadCapacityUnits, spec.WriteCapacityUnits
	default:
		return awserror.Validation("ThroughputMode must be PAY_PER_REQUEST or PROVISIONED")
	}

	table.CapacitySpecification = next
	return nil
}

func checkCounterTTL(table *api.GetTableResponse, types map[string]cqlType) error {
	if table.Ttl != nil {
		for _, ct := range types {
			if ct.counter {
				return awserror.Validation("TTL is unsupported for counter tables")
			}
		}
	}

	return nil
}

func (s *Service) GetTable(ctx context.Context, in *api.GetTableRequest) (*api.GetTableResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.GetTableResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := readRecord(tx, childKey(scope, kindTable, common.Value(in.KeyspaceName), common.Value(in.TableName)))
		if e != nil {
			return e
		}

		out = s.tableOutput(r)
		return nil
	})
	return out, err
}

func (s *Service) tableOutput(r *record) *api.GetTableResponse {
	r.Table.Status = new(s.status(r))
	return r.Table
}

func (s *Service) DeleteTable(ctx context.Context, in *api.DeleteTableRequest) (*api.DeleteTableResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	ks, name := common.Value(in.KeyspaceName), common.Value(in.TableName)
	if writableErr := writable(ks); writableErr != nil {
		return nil, writableErr
	}

	key := childKey(scope, kindTable, ks, name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, e := readRecord(tx, key); e != nil {
			return e
		}

		return s.writeChange(tx, scope, key, nil, "", "DROP TABLE IF EXISTS "+qualified(ks, name))
	})
	return &api.DeleteTableResponse{}, err
}

func (s *Service) ListTables(ctx context.Context, in *api.ListTablesRequest) (*api.ListTablesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	ks := common.Value(in.KeyspaceName)
	tables := []api.TableSummary{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if e := requireKeyspace(tx, scope, ks); e != nil {
			return e
		}

		records, e := storage.Records[record](tx, childKey(scope, kindTable, ks, ""))
		if e != nil {
			return e
		}

		for _, r := range records {
			tables = append(tables, api.TableSummary{
				ResourceArn: new(resourceARN(scope, ks, r.Name)), KeyspaceName: new(ks), TableName: new(r.Name),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := resourcePage(tables, func(t api.TableSummary) string { return common.Value(t.TableName) },
		in.NextToken, in.MaxResults)
	return &api.ListTablesResponse{Tables: page, NextToken: next}, err
}
