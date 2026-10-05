package dynamodb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// An on-demand backup is a copy of its table's record and items, which
// outlives the table:
//
//	<scope>/dynamodb-backup/<table>/<backup ID>
//	<scope>/dynamodb-backup-item/<table>/<backup ID>/<item ID>
//
// A backup ID starts with its zero-padded creation time in milliseconds, so a
// table's backups list in creation order.

const (
	backupUser          = "USER"
	billingOnDemand     = "PAY_PER_REQUEST"
	billingProvisioned  = "PROVISIONED"
	defaultRecoveryDays = 35
	statusEnabled       = "ENABLED"
	statusDisabled      = "DISABLED"
)

var backupName = regexp.MustCompile(`^[A-Za-z0-9_.-]{3,255}$`)

type backup struct {
	Details api.BackupDetails
	// Source is the table record when the backup was made.
	Source table
}

func backupPrefix(scope request.Scope) string {
	return scope.AccountID + "/" + scope.Region + "/dynamodb-backup/"
}

func backupItemPrefix(scope request.Scope, owner, id string) string {
	return scope.AccountID + "/" + scope.Region + "/dynamodb-backup-item/" + owner + "/" + id + "/"
}

func scopeOf(ctx context.Context) (request.Scope, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" || m.Region == "" {
		return request.Scope{}, fmt.Errorf("missing request identity")
	}

	return m.Scope, nil
}

// tableNamed resolves a member that takes a table name or ARN to the table's
// storage key.
func tableNamed(ctx context.Context, nameOrARN *string) (string, error) {
	name := nameOrARN
	if strings.HasPrefix(common.Value(nameOrARN), "arn:") {
		var err error
		if name, err = arnName(ctx, nameOrARN); err != nil {
			return "", err
		}
	}

	return tableKey(ctx, name)
}

// loadSource loads a table that an operation reads from, reporting a missing
// one as TableNotFoundException.
func loadSource(tx storage.ReadTx, key string) (*table, error) {
	t, err := load(tx, key)
	if err != nil {
		_, name := tableScope(key)
		return nil, awserror.New("TableNotFoundException", "Table not found: "+name)
	}

	return t, nil
}

// backupKey resolves a backup ARN of the caller's account and region to its
// storage key, its table, and its ID.
func backupKey(ctx context.Context, arn *string) (key, owner, id string, err error) {
	scope, err := scopeOf(ctx)
	if err != nil {
		return "", "", "", err
	}

	notFound := awserror.New("BackupNotFoundException", "Backup not found: "+common.Value(arn))
	prefix := "arn:aws:dynamodb:" + scope.Region + ":" + scope.AccountID + ":table/"
	rest, ok := strings.CutPrefix(common.Value(arn), prefix)
	owner, id, found := strings.Cut(rest, "/backup/")
	if !ok || !found || !tableName.MatchString(owner) || id == "" || strings.Contains(id, "/") {
		return "", "", "", notFound
	}

	return backupPrefix(scope) + owner + "/" + id, owner, id, nil
}

func loadBackup(tx storage.ReadTx, key, arn string) (*backup, error) {
	data, ok := tx.Get(key)
	if !ok {
		return nil, awserror.New("BackupNotFoundException", "Backup not found: "+arn)
	}

	var b backup
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("decode backup: %w", err)
	}

	return &b, nil
}

func (s *Service) CreateBackup(ctx context.Context, in *api.CreateBackupInput) (*api.CreateBackupOutput, error) {
	key, err := tableNamed(ctx, in.TableName)
	if err != nil {
		return nil, err
	}

	if !backupName.MatchString(common.Value(in.BackupName)) {
		return nil, awserror.Validation("BackupName must be 3 to 255 characters using letters, digits, _, -, or .")
	}

	scope, name := tableScope(key)
	now := time.Now()
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	id := fmt.Sprintf("%013d-%s", now.UnixMilli(), hex.EncodeToString(suffix))
	var b backup
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := loadSource(tx, key)
		if loadErr != nil {
			return loadErr
		}

		b = backup{Source: *t, Details: api.BackupDetails{
			BackupArn: new(common.Value(t.Description.TableArn) + "/backup/" + id), BackupName: in.BackupName,
			BackupSizeBytes: t.Description.TableSizeBytes, BackupStatus: new("AVAILABLE"),
			BackupType: new(backupUser), BackupCreationDateTime: new(float64(now.UnixMilli()) / 1000),
		}}
		items := backupItemPrefix(scope, name, id)
		from := itemRecord(key, "")
		for _, record := range itemRecords(tx, key, "") {
			data, _ := tx.Get(record)
			tx.Put(items+record[len(from):], data)
		}

		return write(tx, backupPrefix(scope)+name+"/"+id, &b)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateBackupOutput{BackupDetails: &b.Details}, nil
}

// describe renders a backup as DescribeBackup reports it.
func (b *backup) describe() *api.BackupDescription {
	d := &b.Source.Description
	mode := common.Value(common.Value(d.BillingModeSummary).BillingMode)
	source := &api.SourceTableDetails{
		TableName: d.TableName, TableId: d.TableId, TableArn: d.TableArn, TableSizeBytes: d.TableSizeBytes,
		KeySchema: d.KeySchema, TableCreationDateTime: d.CreationDateTime, ItemCount: d.ItemCount,
		BillingMode: &mode,
	}
	if p := d.ProvisionedThroughput; mode == billingProvisioned && p != nil {
		source.ProvisionedThroughput = &api.ProvisionedThroughput{
			ReadCapacityUnits: p.ReadCapacityUnits, WriteCapacityUnits: p.WriteCapacityUnits,
		}
	}

	features := &api.SourceTableFeatureDetails{StreamDescription: d.StreamSpecification}
	if b.Source.TTL != "" {
		features.TimeToLiveDescription = &api.TimeToLiveDescription{
			TimeToLiveStatus: new(statusEnabled), AttributeName: new(b.Source.TTL),
		}
	}

	for _, l := range d.LocalSecondaryIndexes {
		features.LocalSecondaryIndexes = append(features.LocalSecondaryIndexes, api.LocalSecondaryIndexInfo{
			IndexName: l.IndexName, KeySchema: l.KeySchema, Projection: l.Projection,
		})
	}

	for _, g := range d.GlobalSecondaryIndexes {
		info := api.GlobalSecondaryIndexInfo{IndexName: g.IndexName, KeySchema: g.KeySchema, Projection: g.Projection}
		if p := g.ProvisionedThroughput; mode == billingProvisioned && p != nil {
			info.ProvisionedThroughput = &api.ProvisionedThroughput{
				ReadCapacityUnits: p.ReadCapacityUnits, WriteCapacityUnits: p.WriteCapacityUnits,
			}
		}

		features.GlobalSecondaryIndexes = append(features.GlobalSecondaryIndexes, info)
	}

	return &api.BackupDescription{
		BackupDetails:             &b.Details,
		SourceTableDetails:        source,
		SourceTableFeatureDetails: features,
	}
}

func (s *Service) DescribeBackup(ctx context.Context, in *api.DescribeBackupInput) (*api.DescribeBackupOutput, error) {
	key, _, _, err := backupKey(ctx, in.BackupArn)
	if err != nil {
		return nil, err
	}

	var b *backup
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		b, loadErr = loadBackup(tx, key, common.Value(in.BackupArn))
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeBackupOutput{BackupDescription: b.describe()}, nil
}

func (s *Service) DeleteBackup(ctx context.Context, in *api.DeleteBackupInput) (*api.DeleteBackupOutput, error) {
	key, owner, id, err := backupKey(ctx, in.BackupArn)
	if err != nil {
		return nil, err
	}

	scope, err := scopeOf(ctx)
	if err != nil {
		return nil, err
	}

	var b *backup
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var loadErr error
		if b, loadErr = loadBackup(tx, key, common.Value(in.BackupArn)); loadErr != nil {
			return loadErr
		}

		tx.Delete(key)
		for _, record := range tx.Keys(backupItemPrefix(scope, owner, id)) {
			tx.Delete(record)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	b.Details.BackupStatus = new("DELETED")
	return &api.DeleteBackupOutput{BackupDescription: b.describe()}, nil
}

// ListBackups lists backups by table and then creation time.
func (s *Service) ListBackups(ctx context.Context, in *api.ListBackupsInput) (*api.ListBackupsOutput, error) {
	scope, err := scopeOf(ctx)
	if err != nil {
		return nil, err
	}

	limit := int64(100)
	switch l := common.Value(in.Limit); {
	case in.Limit != nil && (l < 1 || l > 100):
		return nil, awserror.Validation("Limit must be between 1 and 100")
	case in.Limit != nil:
		limit = l
	}

	switch common.Value(in.BackupType) {
	case "", backupUser, "ALL":
	case "SYSTEM", "AWS_BACKUP":
		return &api.ListBackupsOutput{BackupSummaries: []api.BackupSummary{}}, nil
	default:
		return nil, awserror.Validation("BackupType must be USER, SYSTEM, AWS_BACKUP, or ALL")
	}

	prefix := backupPrefix(scope)
	if in.TableName != nil {
		key, keyErr := tableNamed(ctx, in.TableName)
		if keyErr != nil {
			return nil, keyErr
		}

		_, name := tableScope(key)
		prefix += name + "/"
	}

	after := ""
	if in.ExclusiveStartBackupArn != nil {
		if after, _, _, err = backupKey(ctx, in.ExclusiveStartBackupArn); err != nil {
			return nil, err
		}
	}

	out := &api.ListBackupsOutput{BackupSummaries: []api.BackupSummary{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys(prefix) {
			if key <= after {
				continue
			}

			b, loadErr := loadBackup(tx, key, "")
			if loadErr != nil {
				return loadErr
			}

			created := common.Value(b.Details.BackupCreationDateTime)
			if (in.TimeRangeLowerBound != nil && created < *in.TimeRangeLowerBound) ||
				(in.TimeRangeUpperBound != nil && created >= *in.TimeRangeUpperBound) {
				continue
			}

			if int64(len(out.BackupSummaries)) == limit {
				last := out.BackupSummaries[len(out.BackupSummaries)-1]
				out.LastEvaluatedBackupArn = last.BackupArn
				return nil
			}

			d := &b.Source.Description
			out.BackupSummaries = append(out.BackupSummaries, api.BackupSummary{
				TableName: d.TableName, TableId: d.TableId, TableArn: d.TableArn, BackupArn: b.Details.BackupArn,
				BackupName: b.Details.BackupName, BackupCreationDateTime: b.Details.BackupCreationDateTime,
				BackupStatus: b.Details.BackupStatus, BackupType: b.Details.BackupType,
				BackupSizeBytes: b.Details.BackupSizeBytes,
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// restoreOptions are what a restore changes about its source table.
type restoreOptions struct {
	billing    *string
	throughput *api.ProvisionedThroughput
	summary    api.RestoreSummary
}

// restore creates the table stored under key from src, a table record, and
// copies items, the storage keys of src's items, which hold item IDs after
// from. Like DynamoDB, the new table has no stream, TTL, tags, or
// point-in-time recovery.
func restore(
	tx storage.WriteTx,
	key string,
	src *table,
	items []string,
	from string,
	o *restoreOptions,
) (*table, error) {
	if _, exists := tx.Get(key); exists {
		_, name := tableScope(key)
		return nil, awserror.New("TableAlreadyExistsException", "Table already exists: "+name)
	}

	data, err := json.Marshal(src.Description)
	if err != nil {
		return nil, err
	}

	t := &table{}
	if decodeErr := json.Unmarshal(data, &t.Description); decodeErr != nil {
		return nil, decodeErr
	}

	scope, name := tableScope(key)
	arn := "arn:aws:dynamodb:" + scope.Region + ":" + scope.AccountID + ":table/" + name
	d := &t.Description
	d.TableName, d.TableArn, d.TableId, d.TableStatus = &name, &arn, new(rand.Text()), new("ACTIVE")
	d.CreationDateTime = new(float64(time.Now().UnixMilli()) / 1000)
	d.StreamSpecification, d.LatestStreamArn, d.LatestStreamLabel = nil, nil, nil
	d.RestoreSummary = &o.summary
	for i := range d.GlobalSecondaryIndexes {
		d.GlobalSecondaryIndexes[i].IndexArn = new(
			arn + "/index/" + common.Value(d.GlobalSecondaryIndexes[i].IndexName),
		)
	}

	for i := range d.LocalSecondaryIndexes {
		d.LocalSecondaryIndexes[i].IndexArn = new(arn + "/index/" + common.Value(d.LocalSecondaryIndexes[i].IndexName))
	}

	if billErr := rebill(t, o.billing, o.throughput); billErr != nil {
		return nil, billErr
	}

	to := itemRecord(key, "")
	for _, record := range items {
		data, _ := tx.Get(record)
		tx.Put(to+record[len(from):], data)
	}

	return t, save(tx, key, t)
}

// rebill applies a restore's billing mode and provisioned throughput
// overrides; a provisioned table keeps its throughput unless one is given.
func rebill(t *table, billing *string, throughput *api.ProvisionedThroughput) error {
	d := &t.Description
	mode := common.Value(common.Value(d.BillingModeSummary).BillingMode)
	if billing != nil {
		mode = *billing
	}

	units := func(read, write *int64) *api.ProvisionedThroughputDescription {
		return &api.ProvisionedThroughputDescription{
			ReadCapacityUnits: read, WriteCapacityUnits: write, NumberOfDecreasesToday: new(int64(0)),
		}
	}

	var p *api.ProvisionedThroughputDescription
	switch {
	case mode == billingOnDemand && throughput != nil:
		return awserror.Validation("ProvisionedThroughputOverride is not valid for PAY_PER_REQUEST")
	case mode == billingOnDemand:
		p = units(new(int64(0)), new(int64(0)))
	case mode != billingProvisioned:
		return awserror.Validation("BillingModeOverride must be PROVISIONED or PAY_PER_REQUEST")
	case throughput != nil:
		if common.Value(throughput.ReadCapacityUnits) < 1 || common.Value(throughput.WriteCapacityUnits) < 1 {
			return awserror.Validation("Positive provisioned read and write capacity required")
		}

		p = units(throughput.ReadCapacityUnits, throughput.WriteCapacityUnits)
	case common.Value(d.ProvisionedThroughput).ReadCapacityUnits == nil ||
		common.Value(common.Value(d.ProvisionedThroughput).ReadCapacityUnits) < 1:
		return awserror.Validation("ProvisionedThroughputOverride is required to restore an on-demand table as " +
			"PROVISIONED")
	default:
		return nil
	}

	d.BillingModeSummary = &api.BillingModeSummary{BillingMode: &mode}
	d.ProvisionedThroughput = p
	for i := range d.GlobalSecondaryIndexes {
		d.GlobalSecondaryIndexes[i].ProvisionedThroughput = units(p.ReadCapacityUnits, p.WriteCapacityUnits)
	}

	return nil
}

func (s *Service) RestoreTableFromBackup(
	ctx context.Context,
	in *api.RestoreTableFromBackupInput,
) (*api.RestoreTableFromBackupOutput, error) {
	key, err := tableKey(ctx, in.TargetTableName)
	if err != nil {
		return nil, err
	}

	backupRecord, owner, id, err := backupKey(ctx, in.BackupArn)
	if err != nil {
		return nil, err
	}

	scope, _ := tableScope(key)
	var t *table
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		b, loadErr := loadBackup(tx, backupRecord, common.Value(in.BackupArn))
		if loadErr != nil {
			return loadErr
		}

		items := backupItemPrefix(scope, owner, id)
		var restoreErr error
		t, restoreErr = restore(tx, key, &b.Source, tx.Keys(items), items, &restoreOptions{
			billing: in.BillingModeOverride, throughput: in.ProvisionedThroughputOverride,
			summary: api.RestoreSummary{
				SourceBackupArn: in.BackupArn, SourceTableArn: b.Source.Description.TableArn,
				RestoreDateTime: b.Details.BackupCreationDateTime, RestoreInProgress: new(false),
			},
		})
		return restoreErr
	})
	if err != nil {
		return nil, err
	}

	return &api.RestoreTableFromBackupOutput{TableDescription: &t.Description}, nil
}

// RestoreTableToPointInTime restores a table with point-in-time recovery as
// it is now: TinyStack keeps no history, so it restores only the latest
// restorable time.
func (s *Service) RestoreTableToPointInTime(
	ctx context.Context,
	in *api.RestoreTableToPointInTimeInput,
) (*api.RestoreTableToPointInTimeOutput, error) {
	source := in.SourceTableName
	switch {
	case (in.SourceTableName == nil) == (in.SourceTableArn == nil):
		return nil, awserror.Validation("Exactly one of SourceTableName and SourceTableArn must be given")
	case in.SourceTableArn != nil:
		source = in.SourceTableArn
	}

	if in.RestoreDateTime != nil || !common.Value(in.UseLatestRestorableTime) {
		return nil, awserror.New("InvalidRestoreTimeException",
			"TinyStack restores only the latest restorable time; set UseLatestRestorableTime")
	}

	sourceKey, err := tableNamed(ctx, source)
	if err != nil {
		return nil, err
	}

	key, err := tableKey(ctx, in.TargetTableName)
	if err != nil {
		return nil, err
	}

	var t *table
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		src, loadErr := loadSource(tx, sourceKey)
		if loadErr != nil {
			return loadErr
		}

		if src.PITR == 0 {
			return awserror.New("PointInTimeRecoveryUnavailableException",
				"Point in time recovery is not enabled for table '"+common.Value(src.Description.TableName)+"'")
		}

		var restoreErr error
		t, restoreErr = restore(tx, key, src, itemRecords(tx, sourceKey, ""), itemRecord(sourceKey, ""),
			&restoreOptions{
				billing: in.BillingModeOverride, throughput: in.ProvisionedThroughputOverride,
				summary: api.RestoreSummary{
					SourceTableArn: src.Description.TableArn, RestoreInProgress: new(false),
					RestoreDateTime: new(float64(time.Now().UnixMilli()) / 1000),
				},
			})
		return restoreErr
	})
	if err != nil {
		return nil, err
	}

	return &api.RestoreTableToPointInTimeOutput{TableDescription: &t.Description}, nil
}

// continuousBackups describes a table's continuous backups: always on, with
// point-in-time recovery when it is enabled.
func continuousBackups(t *table) *api.ContinuousBackupsDescription {
	pitr := &api.PointInTimeRecoveryDescription{PointInTimeRecoveryStatus: new(statusDisabled)}
	if t.PITR != 0 {
		pitr = &api.PointInTimeRecoveryDescription{
			PointInTimeRecoveryStatus: new(statusEnabled), RecoveryPeriodInDays: new(t.PITRDays),
			EarliestRestorableDateTime: new(t.PITR),
			LatestRestorableDateTime:   new(float64(time.Now().UnixMilli()) / 1000),
		}
	}

	return &api.ContinuousBackupsDescription{
		ContinuousBackupsStatus: new(statusEnabled), PointInTimeRecoveryDescription: pitr,
	}
}

func (s *Service) UpdateContinuousBackups(
	ctx context.Context,
	in *api.UpdateContinuousBackupsInput,
) (*api.UpdateContinuousBackupsOutput, error) {
	key, err := tableNamed(ctx, in.TableName)
	if err != nil {
		return nil, err
	}

	spec := in.PointInTimeRecoverySpecification
	if spec == nil || spec.PointInTimeRecoveryEnabled == nil {
		return nil, awserror.Validation("PointInTimeRecoverySpecification.PointInTimeRecoveryEnabled is required")
	}

	days := int64(defaultRecoveryDays)
	if spec.RecoveryPeriodInDays != nil {
		if days = *spec.RecoveryPeriodInDays; days < 1 || days > defaultRecoveryDays {
			return nil, awserror.Validation("RecoveryPeriodInDays must be from 1 to 35")
		}
	}

	out := &api.UpdateContinuousBackupsOutput{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := loadSource(tx, key)
		if loadErr != nil {
			return loadErr
		}

		switch {
		case !*spec.PointInTimeRecoveryEnabled:
			t.PITR, t.PITRDays = 0, 0
		case t.PITR == 0:
			t.PITR, t.PITRDays = float64(time.Now().UnixMilli())/1000, days
		default:
			t.PITRDays = days
		}

		out.ContinuousBackupsDescription = continuousBackups(t)
		return save(tx, key, t)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func write(tx storage.WriteTx, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}

	tx.Put(key, data)
	return nil
}
