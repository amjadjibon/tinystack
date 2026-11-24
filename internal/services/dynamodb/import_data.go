package dynamodb

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

type importedItem struct {
	data []byte
	size int64
}

type importResult struct {
	items                    map[string]importedItem
	firstError               string
	bytes, processed, errors int64
}

func (r *importResult) invalid(err error) {
	r.errors++
	if r.firstError == "" {
		r.firstError = err.Error()
	}
}

func (r *importResult) add(t *table, it item, err error) {
	r.processed++
	if err == nil {
		err = checkItem(t, it, "Item size has exceeded the maximum allowed size")
	}

	if err != nil {
		r.invalid(err)
		return
	}

	id, err := itemKey(t, it, false)
	if err != nil {
		r.invalid(err)
		return
	}

	data, err := json.Marshal(it)
	if err != nil {
		r.invalid(err)
		return
	}

	size, _ := itemSize(it)
	r.items[id] = importedItem{data: data, size: size}
}

func (s *Service) readImportObject(
	ctx context.Context,
	job *importJob,
	entry *s3api.Object,
	result *importResult,
) error {
	d := &job.Description
	src := d.S3BucketSource
	payload := &request.Payload{}
	obj, err := s.s3.GetObject(request.WithPayload(ctx, payload), &s3api.GetObjectRequest{
		Bucket: src.S3Bucket, Key: entry.Key, ExpectedBucketOwner: src.S3BucketOwner, IfMatch: entry.ETag,
	})
	if err != nil {
		return err
	}

	defer func() { _ = payload.Body.Close() }()
	result.bytes += common.Value(obj.ContentLength)
	reader, closeReader, err := importReader(payload.Body, common.Value(d.InputCompressionType))
	if err != nil {
		result.invalid(fmt.Errorf("decode %s: %w", *entry.Key, err))
		return nil
	}

	defer closeReader()
	if common.Value(d.InputFormat) == importCSV {
		return importCSVRows(ctx, reader, common.Value(d.InputFormatOptions).Csv, job.Table, result)
	}

	return importJSONRows(ctx, reader, job.Table, result)
}

func importReader(r io.Reader, compression string) (io.Reader, func(), error) {
	switch compression {
	case "GZIP":
		reader, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, err
		}

		return reader, func() { _ = reader.Close() }, nil
	case "ZSTD":
		reader, err := zstd.NewReader(r, zstd.WithDecoderMaxMemory(64<<20), zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, nil, err
		}

		return reader, reader.Close, nil
	default:
		return r, func() {}, nil
	}
}

func importJSONRows(ctx context.Context, r io.Reader, t *table, result *importResult) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}

		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var record struct {
			Item item
		}

		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		err := decoder.Decode(&record)
		if err == nil && (record.Item == nil || decoder.Decode(new(json.RawMessage)) != io.EOF) {
			err = awserror.Validation("Expected one DynamoDB JSON Item on the line")
		}

		result.add(t, record.Item, err)
	}

	if err := scanner.Err(); err != nil {
		result.invalid(err)
	}

	return nil
}

func validateHeaders(headers []string) error {
	if len(headers) == 0 || len(headers) > 255 {
		return awserror.Validation("CSV needs 1 to 255 column headers")
	}

	seen := make(map[string]bool, len(headers))
	for _, name := range headers {
		if name == "" || seen[name] {
			return awserror.Validation("CSV headers must be nonempty and unique")
		}

		seen[name] = true
	}

	return nil
}

func importCSVRows(ctx context.Context, r io.Reader, opts *api.CsvOptions, t *table, result *importResult) error {
	options := common.Value(opts)
	reader := csv.NewReader(r)
	if options.Delimiter != nil {
		reader.Comma = rune((*options.Delimiter)[0])
	}

	headers := options.HeaderList
	if headers == nil {
		var err error
		headers, err = reader.Read()
		if err != nil {
			result.invalid(fmt.Errorf("read CSV header: %w", err))
			return nil
		}
	}

	if err := validateHeaders(headers); err != nil {
		result.invalid(err)
		return nil
	}

	reader.FieldsPerRecord = len(headers)
	kinds := make(map[string]string, len(t.Description.AttributeDefinitions))
	for _, def := range t.Description.AttributeDefinitions {
		kinds[*def.AttributeName] = *def.AttributeType
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		fields, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			result.add(t, nil, err)
			var parseErr *csv.ParseError
			if !errors.As(err, &parseErr) {
				return nil
			}

			continue
		}

		values, err := csvItem(headers, fields, kinds)
		result.add(t, values, err)
	}
}

func csvItem(headers, fields []string, kinds map[string]string) (item, error) {
	values := make(item, len(fields))
	for i, field := range fields {
		if field == "" {
			continue
		}

		name := headers[i]
		switch kinds[name] {
		case "N":
			values[name] = api.AttributeValue{N: new(field)}
		case "B":
			raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(field))
			if err != nil {
				return nil, fmt.Errorf("CSV key %s is not base64: %w", name, err)
			}

			values[name] = api.AttributeValue{B: &raw}
		default:
			values[name] = api.AttributeValue{S: new(field)}
		}
	}

	return values, nil
}
