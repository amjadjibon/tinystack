package dynamodb

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5" // #nosec G501 -- the export manifest's checksum is MD5, as DynamoDB writes it
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// An export writes a table's items to S3, through the catalog.Caller, in
// DynamoDB's layout: <prefix>/AWSDynamoDB/<export ID>/ holds _started,
// manifest-summary.json, manifest-files.json, and one gzipped data file of
// {"Item": ...} lines. Exports run to completion inside the request and are
// kept under <scope>/dynamodb-export/<table>/<export ID>.

const (
	exportCompleted  = "COMPLETED"
	exportFailed     = "FAILED"
	exportFull       = "FULL_EXPORT"
	formatJSON       = "DYNAMODB_JSON"
	manifestVersion  = "2020-06-30"
	maxExportResults = 25
)

// UseCaller lets table exports write to S3. Call it before serving requests.
func (s *Service) UseCaller(caller *catalog.Caller) { s.caller = caller }

func exportPrefix(scope request.Scope) string {
	return scope.AccountID + "/" + scope.Region + "/dynamodb-export/"
}

// exportKey resolves an export ARN of the caller's account and region to its
// storage key.
func exportKey(ctx context.Context, arn *string) (string, error) {
	scope, err := scopeOf(ctx)
	if err != nil {
		return "", err
	}

	rest, ok := strings.CutPrefix(common.Value(arn), "arn:aws:dynamodb:"+scope.Region+":"+scope.AccountID+":table/")
	owner, id, found := strings.Cut(rest, "/export/")
	if !ok || !found || !tableName.MatchString(owner) || id == "" || strings.Contains(id, "/") {
		return "", awserror.New("ExportNotFoundException", "Export not found: "+common.Value(arn))
	}

	return exportPrefix(scope) + owner + "/" + id, nil
}

func loadExport(tx storage.ReadTx, key string) (*api.ExportDescription, error) {
	data, ok := tx.Get(key)
	if !ok {
		return nil, awserror.New("ExportNotFoundException", "Export not found")
	}

	var d api.ExportDescription
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("decode export: %w", err)
	}

	return &d, nil
}

// ExportTableToPointInTime writes a table with point-in-time recovery to S3
// as it is now: TinyStack keeps no history, so ExportTime must be omitted.
// A repeated ClientToken returns the export it started.
func (s *Service) ExportTableToPointInTime(
	ctx context.Context,
	in *api.ExportTableToPointInTimeInput,
) (*api.ExportTableToPointInTimeOutput, error) {
	switch {
	case in.ExportTime != nil:
		return nil, awserror.New("InvalidExportTimeException",
			"TinyStack exports only the current state of a table; omit ExportTime")
	case common.Value(in.S3Bucket) == "":
		return nil, awserror.Validation("S3Bucket is required")
	case in.ExportFormat != nil && *in.ExportFormat != formatJSON:
		return nil, awserror.Validation("TinyStack supports only the DYNAMODB_JSON export format")
	case in.ExportType != nil && *in.ExportType != exportFull:
		return nil, awserror.Validation("TinyStack supports only FULL_EXPORT exports")
	case s.caller == nil:
		return nil, awserror.New("InternalServerError", "Exports cannot reach S3")
	}

	if !strings.HasPrefix(common.Value(in.TableArn), "arn:") {
		return nil, awserror.Validation("Invalid TableArn: %s", common.Value(in.TableArn))
	}

	key, err := tableNamed(ctx, in.TableArn)
	if err != nil {
		return nil, err
	}

	scope, name := tableScope(key)
	var d *api.ExportDescription
	var lines bytes.Buffer
	count := int64(0)
	start := time.Now()
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		t, loadErr := loadSource(tx, key)
		if loadErr != nil {
			return loadErr
		}

		if t.PITR == 0 {
			return awserror.New("PointInTimeRecoveryUnavailableException",
				"Point in time recovery is not enabled for table '"+name+"'")
		}

		if d, loadErr = findExport(tx, exportPrefix(scope)+name+"/", in); d != nil || loadErr != nil {
			return loadErr
		}

		suffix := make([]byte, 4)
		_, _ = rand.Read(suffix)
		id := fmt.Sprintf("%014d-%s", start.UnixMilli(), hex.EncodeToString(suffix))
		d = &api.ExportDescription{
			ExportArn: new(common.Value(t.Description.TableArn) + "/export/" + id), ExportStatus: new(exportCompleted),
			StartTime: new(float64(start.UnixMilli()) / 1000), TableArn: t.Description.TableArn,
			TableId: t.Description.TableId, ExportTime: new(float64(start.UnixMilli()) / 1000),
			ClientToken: in.ClientToken, S3Bucket: in.S3Bucket, S3Prefix: in.S3Prefix,
			ExportFormat: new(formatJSON), ExportType: new(exportFull),
			BilledSizeBytes: t.Description.TableSizeBytes,
		}
		for _, record := range itemRecords(tx, key, "") {
			it, _, itemErr := loadItem(tx, record)
			if itemErr != nil {
				return itemErr
			}

			line, encodeErr := json.Marshal(map[string]item{"Item": it})
			if encodeErr != nil {
				return encodeErr
			}

			lines.Write(append(line, '\n'))
			count++
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if d.EndTime != nil {
		return &api.ExportTableToPointInTimeOutput{ExportDescription: d}, nil
	}

	d.ItemCount = &count
	if writeErr := s.writeExport(ctx, scope, d, lines.Bytes(), count); writeErr != nil {
		d.ExportStatus, d.FailureCode, d.FailureMessage = new(exportFailed), new("S3"), new(writeErr.Error())
		var awsErr *awserror.Error
		if errors.As(writeErr, &awsErr) {
			d.FailureCode, d.FailureMessage = new("S3"+awsErr.Code), new(awsErr.Message)
		}
	}

	d.EndTime = new(float64(time.Now().UnixMilli()) / 1000)
	_, id, _ := strings.Cut(common.Value(d.ExportArn), "/export/")
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return write(tx, exportPrefix(scope)+name+"/"+id, d)
	})
	if err != nil {
		return nil, err
	}

	return &api.ExportTableToPointInTimeOutput{ExportDescription: d}, nil
}

// findExport returns the export of a table, whose exports are under prefix,
// that in's ClientToken started, or nil when there is none; the token must
// not be reused for a different export.
func findExport(
	tx storage.ReadTx,
	prefix string,
	in *api.ExportTableToPointInTimeInput,
) (*api.ExportDescription, error) {
	if in.ClientToken == nil {
		return nil, nil //nolint:nilnil // without a token, every request is a new export
	}

	for _, key := range tx.Keys(prefix) {
		d, err := loadExport(tx, key)
		if err != nil || common.Value(d.ClientToken) != *in.ClientToken {
			continue
		}

		if common.Value(d.S3Bucket) != common.Value(in.S3Bucket) ||
			common.Value(d.S3Prefix) != common.Value(in.S3Prefix) {
			return nil, awserror.New("ExportConflictException",
				"An export with this ClientToken was requested with different parameters")
		}

		return d, nil
	}

	return nil, nil //nolint:nilnil // the token started no export
}

// writeExport writes an export's objects to its bucket.
func (s *Service) writeExport(
	ctx context.Context,
	scope request.Scope,
	d *api.ExportDescription,
	lines []byte,
	count int64,
) error {
	_, id, _ := strings.Cut(common.Value(d.ExportArn), "/export/")
	base := "AWSDynamoDB/" + id
	if prefix := strings.Trim(common.Value(d.S3Prefix), "/"); prefix != "" {
		base = prefix + "/" + base
	}

	var zipped bytes.Buffer
	w := gzip.NewWriter(&zipped)
	if _, err := w.Write(lines); err != nil {
		return err
	}

	if err := w.Close(); err != nil {
		return err
	}

	name := make([]byte, 16)
	_, _ = rand.Read(name)
	dataKey := base + "/data/" + strings.ToLower(base64.RawURLEncoding.EncodeToString(name)) + ".json.gz"
	sum := md5.Sum(zipped.Bytes()) // #nosec G401 -- see the import
	files, err := json.Marshal(map[string]any{
		"itemCount": count, "md5Checksum": base64.StdEncoding.EncodeToString(sum[:]),
		"etag": hex.EncodeToString(sum[:]), "dataFileS3Key": dataKey,
	})
	if err != nil {
		return err
	}

	d.ExportManifest = new(base + "/manifest-summary.json")
	summary, err := json.Marshal(map[string]any{
		"version": manifestVersion, "exportArn": d.ExportArn, "startTime": exportTime(d.StartTime),
		"endTime": exportTime(new(float64(time.Now().UnixMilli()) / 1000)), "tableArn": d.TableArn,
		"tableId": d.TableId, "exportTime": exportTime(d.ExportTime), "s3Bucket": d.S3Bucket,
		"s3Prefix": d.S3Prefix, "s3SseAlgorithm": nil, "s3SseKmsKeyId": nil,
		"manifestFilesS3Key": base + "/manifest-files.json", "billedSizeBytes": common.Value(d.BilledSizeBytes),
		"itemCount": count, "outputFormat": formatJSON,
	})
	if err != nil {
		return err
	}

	ctx = request.WithMetadata(context.WithoutCancel(ctx), request.Metadata{Scope: scope, Service: "s3"})
	for _, object := range []struct {
		key  string
		body []byte
	}{
		{base + "/_started", nil},
		{dataKey, zipped.Bytes()},
		{base + "/manifest-files.json", append(files, '\n')},
		{*d.ExportManifest, summary},
	} {
		if _, err = s.caller.Call(
			ctx,
			"s3",
			"PutObject",
			map[string]any{
				"Bucket": common.Value(d.S3Bucket),
				"Key":    object.key,
				"Body":   string(object.body),
			},
		); err != nil {
			return err
		}
	}

	return nil
}

// exportTime formats a manifest's times, which are ISO 8601.
func exportTime(seconds *float64) string {
	return time.UnixMilli(int64(common.Value(seconds) * 1000)).UTC().Format("2006-01-02T15:04:05.000Z")
}

func (s *Service) DescribeExport(ctx context.Context, in *api.DescribeExportInput) (*api.DescribeExportOutput, error) {
	key, err := exportKey(ctx, in.ExportArn)
	if err != nil {
		return nil, err
	}

	var d *api.ExportDescription
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		d, loadErr = loadExport(tx, key)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeExportOutput{ExportDescription: d}, nil
}

// ListExports lists exports by table and then start time.
func (s *Service) ListExports(ctx context.Context, in *api.ListExportsInput) (*api.ListExportsOutput, error) {
	scope, err := scopeOf(ctx)
	if err != nil {
		return nil, err
	}

	limit := int64(maxExportResults)
	if in.MaxResults != nil {
		if limit = *in.MaxResults; limit < 1 || limit > maxExportResults {
			return nil, awserror.Validation("MaxResults must be from 1 to %d", maxExportResults)
		}
	}

	prefix := exportPrefix(scope)
	if in.TableArn != nil {
		key, keyErr := tableNamed(ctx, in.TableArn)
		if keyErr != nil {
			return nil, keyErr
		}

		_, name := tableScope(key)
		prefix += name + "/"
	}

	after := ""
	if in.NextToken != nil {
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(*in.NextToken)
		if decodeErr != nil || !strings.HasPrefix(string(decoded), exportPrefix(scope)) {
			return nil, awserror.Validation("Invalid NextToken")
		}

		after = string(decoded)
	}

	out := &api.ListExportsOutput{ExportSummaries: []api.ExportSummary{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys(prefix) {
			if key <= after {
				continue
			}

			if int64(len(out.ExportSummaries)) == limit {
				out.NextToken = new(base64.RawURLEncoding.EncodeToString([]byte(after)))
				return nil
			}

			d, loadErr := loadExport(tx, key)
			if loadErr != nil {
				return loadErr
			}

			out.ExportSummaries = append(out.ExportSummaries, api.ExportSummary{
				ExportArn: d.ExportArn, ExportStatus: d.ExportStatus, ExportType: d.ExportType,
			})
			after = key
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
