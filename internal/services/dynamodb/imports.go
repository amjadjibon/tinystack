package dynamodb

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	importIndex        = "aws/dynamodb/index/import/"
	importRunning      = "IN_PROGRESS"
	importFailed       = "FAILED"
	importComplete     = "COMPLETED"
	importCSV          = "CSV"
	importPageLimit    = 25
	importTokenTimeout = 8 * time.Hour
)

type importSource interface {
	ListObjectsV2(context.Context, *s3api.ListObjectsV2Request) (*s3api.ListObjectsV2Output, error)
	GetObject(context.Context, *s3api.GetObjectRequest) (*s3api.GetObjectOutput, error)
}

type importJob struct {
	Description api.ImportTableDescription
	Table       *table
}

// UseS3 starts the worker for pending imports. Call it before serving requests.
func (s *Service) UseS3(buckets importSource) {
	s.s3 = buckets
	s.bg.Every(time.Second, s.runImports)
}

func importPrefix(scope request.Scope) string {
	return scope.AccountID + "/" + scope.Region + "/dynamodb-import/"
}

func importReservation(key string) string { return key + "/import-reservation" }

// tableNameTaken covers both a live table and a name reserved by an import.
func tableNameTaken(tx storage.ReadTx, key string) bool {
	if _, exists := tx.Get(key); exists {
		return true
	}

	_, reserved := tx.Get(importReservation(key))
	return reserved
}

func importRecord(scope request.Scope, arn string) (string, error) {
	prefix := "arn:aws:dynamodb:" + scope.Region + ":" + scope.AccountID + ":table/"
	suffix, ok := strings.CutPrefix(arn, prefix)
	name, id, found := strings.Cut(suffix, "/import/")
	if !ok || !found || !tableName.MatchString(name) || id == "" || strings.Contains(id, "/") {
		return "", awserror.New("ImportNotFoundException", "Import not found: "+arn)
	}

	return importPrefix(scope) + id, nil
}

func (s *Service) ImportTable(ctx context.Context, in *api.ImportTableInput) (*api.ImportTableOutput, error) {
	if err := validateImport(in); err != nil {
		return nil, err
	}

	if s.s3 == nil {
		return nil, awserror.New("InternalServerError", "Imports cannot reach S3")
	}

	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	p := in.TableCreationParameters
	if p.TableName == nil || !tableName.MatchString(*p.TableName) {
		return nil, awserror.Validation("Invalid TableCreationParameters.TableName")
	}

	tableKey, err := tableKey(ctx, p.TableName)
	if err != nil {
		return nil, err
	}

	t, err := prepareTable(ctx, p.TableName, &api.CreateTableInput{
		AttributeDefinitions: p.AttributeDefinitions, KeySchema: p.KeySchema, BillingMode: p.BillingMode,
		ProvisionedThroughput: p.ProvisionedThroughput, GlobalSecondaryIndexes: p.GlobalSecondaryIndexes,
	})
	if err != nil {
		return nil, err
	}

	now := s.now()
	id := fmt.Sprintf("%014d-%s", now.UnixMilli(), rand.Text())
	d := &api.ImportTableDescription{
		ImportArn: new(*t.Description.TableArn + "/import/" + id), ImportStatus: new(importRunning),
		TableArn: t.Description.TableArn, TableId: t.Description.TableId, ClientToken: in.ClientToken,
		InputFormat: in.InputFormat, InputFormatOptions: in.InputFormatOptions,
		InputCompressionType:    common.OrElse(in.InputCompressionType, new(modeNone)),
		TableCreationParameters: p, S3BucketSource: in.S3BucketSource, StartTime: common.SecondsMilli(now),
		ProcessedItemCount: new(int64(0)), ImportedItemCount: new(int64(0)),
		ProcessedSizeBytes: new(int64(0)), ErrorCount: new(int64(0)),
	}
	record := importPrefix(m.Scope) + id
	token := idempotency.Key(m.Scope, "dynamodb", "ImportTable", in.ClientToken)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		prior, found, replayErr := idempotency.Replay[string](tx, token, in,
			awserror.New("ImportConflictException", "An import with this ClientToken has different parameters"))
		if replayErr != nil {
			return replayErr
		}

		if found {
			job, lookupErr := loadImport(tx, prior)
			if lookupErr == nil {
				d = &job.Description
			}

			return lookupErr
		}

		if tableNameTaken(tx, tableKey) {
			return awserror.New("ResourceInUseException", "Table already exists")
		}

		if writeErr := storage.Write(tx, record, &importJob{Description: *d, Table: t}); writeErr != nil {
			return writeErr
		}

		index := importIndex + m.AccountID + "/" + m.Region + "/" + id
		tx.Put(index, []byte(record))
		tx.Put(importReservation(tableKey), []byte(record))
		return idempotency.Remember(tx, token, in, record, now.Add(importTokenTimeout))
	})
	if err != nil {
		return nil, err
	}

	return &api.ImportTableOutput{ImportTableDescription: d}, nil
}

func validateImport(in *api.ImportTableInput) error {
	switch {
	case in.S3BucketSource == nil || common.Value(in.S3BucketSource.S3Bucket) == "":
		return awserror.Validation("S3BucketSource.S3Bucket is required")
	case in.TableCreationParameters == nil:
		return awserror.Validation("TableCreationParameters is required")
	case len(common.Value(in.ClientToken)) > 36:
		return awserror.Validation("ClientToken exceeds 36 characters")
	case common.Value(in.InputFormat) != importCSV && common.Value(in.InputFormat) != formatJSON:
		return awserror.Validation("InputFormat must be CSV or DYNAMODB_JSON")
	case in.InputFormatOptions != nil && common.Value(in.InputFormat) != importCSV:
		return awserror.Validation("InputFormatOptions can only be specified for CSV imports")
	}

	switch *common.OrElse(in.InputCompressionType, new(modeNone)) {
	case modeNone, "GZIP", "ZSTD":
	default:
		return awserror.Validation("InputCompressionType must be NONE, GZIP or ZSTD")
	}

	opts := common.Value(in.InputFormatOptions).Csv
	if opts != nil {
		if d := opts.Delimiter; d != nil && (len(*d) != 1 || !strings.ContainsRune(",;:|\t ", rune((*d)[0]))) {
			return awserror.Validation("Invalid CSV delimiter")
		}

		if opts.HeaderList != nil {
			return validateHeaders(opts.HeaderList)
		}
	}

	return nil
}

func loadImport(tx storage.ReadTx, record string) (*importJob, error) {
	job, found, err := storage.Read[importJob](tx, record)
	if err == nil && !found {
		err = awserror.New("ImportNotFoundException", "Import not found")
	}

	return job, err
}

func (s *Service) DescribeImport(ctx context.Context, in *api.DescribeImportInput) (*api.DescribeImportOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	record, err := importRecord(scope, common.Value(in.ImportArn))
	if err != nil {
		return nil, err
	}

	out := &api.DescribeImportOutput{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		job, loadErr := loadImport(tx, record)
		if loadErr != nil {
			return loadErr
		}

		if common.Value(job.Description.ImportArn) != common.Value(in.ImportArn) {
			return awserror.New("ImportNotFoundException", "Import not found")
		}

		out.ImportTableDescription = &job.Description
		return nil
	})
	return out, err
}

func (s *Service) ListImports(ctx context.Context, in *api.ListImportsInput) (*api.ListImportsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in.TableArn != nil {
		if !strings.HasPrefix(*in.TableArn, "arn:") {
			return nil, awserror.Validation("Invalid TableArn")
		}

		if _, arnErr := tableNamed(ctx, in.TableArn); arnErr != nil {
			return nil, arnErr
		}
	}

	limit := int64(importPageLimit)
	if in.PageSize != nil {
		limit = *in.PageSize
	}

	if limit < 1 || limit > importPageLimit {
		return nil, awserror.Validation("PageSize must be from 1 to %d", importPageLimit)
	}

	var records []string
	jobs := map[string]api.ImportSummary{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, record := range tx.Keys(importPrefix(scope)) {
			job, loadErr := loadImport(tx, record)
			if loadErr != nil {
				return loadErr
			}

			d := &job.Description
			if in.TableArn != nil && *in.TableArn != *d.TableArn {
				continue
			}

			records = append(records, record)
			jobs[record] = api.ImportSummary{
				ImportArn:      d.ImportArn,
				ImportStatus:   d.ImportStatus,
				TableArn:       d.TableArn,
				S3BucketSource: d.S3BucketSource,
				InputFormat:    d.InputFormat,
				StartTime:      d.StartTime,
				EndTime:        d.EndTime,
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(records, func(record string) string { return record }, in.NextToken, int(limit),
		awserror.Validation("Invalid NextToken"))
	if err != nil {
		return nil, err
	}

	out := &api.ListImportsOutput{NextToken: next, ImportSummaryList: make([]api.ImportSummary, 0, len(page))}
	for _, record := range page {
		out.ImportSummaryList = append(out.ImportSummaryList, jobs[record])
	}

	return out, nil
}

func (s *Service) runImports(ctx context.Context) {
	var indexes []string
	_ = s.store.View(ctx, func(tx storage.ReadTx) error {
		indexes = tx.Keys(importIndex)
		return nil
	})

	for _, index := range indexes {
		if ctx.Err() != nil {
			return
		}

		if err := s.runImport(ctx, index); err != nil && ctx.Err() == nil {
			s.logger.Warn("run DynamoDB import", "index", index, "error", err)
		}
	}
}

func (s *Service) runImport(ctx context.Context, index string) error {
	var job *importJob
	var record string
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		value, found := tx.Get(index)
		if !found {
			return nil
		}

		record = string(value)
		var loadErr error
		job, loadErr = loadImport(tx, record)
		return loadErr
	})
	if err != nil || job == nil {
		return err
	}

	account, rest, _ := strings.Cut(strings.TrimPrefix(index, importIndex), "/")
	region, _, _ := strings.Cut(rest, "/")
	scope := request.Scope{AccountID: account, Region: region}
	ctx = request.WithMetadata(ctx, request.Metadata{Scope: scope, Service: "s3"})
	d := &job.Description
	result := &importResult{items: map[string]importedItem{}}
	readErr := s.readImportObjects(ctx, job, result)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found := tx.Get(index); !found {
			return nil
		}

		key := scope.AccountID + "/" + scope.Region + "/dynamodb/" + *job.Table.Description.TableName
		d.ImportStatus = new(importComplete)
		d.ProcessedSizeBytes = &result.bytes
		d.ProcessedItemCount = &result.processed
		d.ErrorCount = &result.errors
		d.EndTime = common.SecondsMilli(s.now())
		switch {
		case readErr != nil:
			d.ImportStatus = new(importFailed)
			d.FailureCode, d.FailureMessage = new("S3ImportError"), new(readErr.Error())
		default:
			if _, exists := tx.Get(key); exists {
				d.ImportStatus = new(importFailed)
				d.FailureCode, d.FailureMessage = new("ResourceInUseException"), new("Table already exists")
				break
			}

			if result.errors != 0 {
				d.ImportStatus = new(importFailed)
				d.FailureCode = new("ValidationException")
				d.FailureMessage = new(result.firstError)
			}

			if err := saveImportedTable(tx, key, job.Table, result.items); err != nil {
				return err
			}

			d.ImportedItemCount = new(int64(len(result.items)))
		}

		tx.Delete(index)
		tx.Delete(importReservation(key))
		job.Table = nil
		return storage.Write(tx, record, job)
	})
}

func saveImportedTable(tx storage.WriteTx, key string, t *table, items map[string]importedItem) error {
	size := int64(0)
	for id, it := range items {
		tx.Put(itemRecord(key, id), it.data)
		size += it.size
	}

	t.Description.ItemCount, t.Description.TableSizeBytes = new(int64(len(items))), &size
	return save(tx, key, t)
}

func (s *Service) readImportObjects(ctx context.Context, job *importJob, result *importResult) error {
	src := job.Description.S3BucketSource
	list := &s3api.ListObjectsV2Request{
		Bucket:              src.S3Bucket,
		Prefix:              src.S3KeyPrefix,
		ExpectedBucketOwner: src.S3BucketOwner,
	}
	found := false
	for {
		page, err := s.s3.ListObjectsV2(ctx, list)
		if err != nil {
			return err
		}

		for i := range page.Contents {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}

			entry := &page.Contents[i]
			if readErr := s.readImportObject(ctx, job, entry, result); readErr != nil {
				return readErr
			}

			found = true
		}

		if page.NextContinuationToken == nil {
			break
		}

		list.ContinuationToken = page.NextContinuationToken
	}

	if !found {
		return awserror.Validation("No S3 objects matched the source prefix")
	}

	return nil
}
