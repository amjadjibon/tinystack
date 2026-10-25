package sfn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strings"

	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

func mapNumber(e *env, n number, path, field string, input any, fallback, low, high float64,
	integer bool,
) (float64, error) {
	v := fallback
	var err error
	if path != "" {
		var selected any
		selected, err = resolvePath(path, input, e)
		if err != nil {
			return 0, err
		}

		var ok bool
		v, ok = selected.(float64)
		if !ok {
			return 0, &stateError{Name: errRuntime, Cause: field + "Path must select a number"}
		}
	} else if n.set {
		v, err = e.number(n, field, input)
		if err != nil {
			return 0, err
		}
	}

	if math.IsNaN(v) || math.IsInf(v, 0) || v < low || v > high || (integer && math.Trunc(v) != v) {
		return 0, &stateError{Name: errRuntime, Cause: "Invalid " + field}
	}

	return v, nil
}

func mapParameters(s *state, params, args, input any, e *env, allowed ...string) (map[string]any, error) {
	var v any
	var err error
	if s.jsonata {
		v, err = e.evaluate(args, map[string]any{keyInput: input})
	} else {
		v, err = template(params, input, e)
	}

	if err != nil {
		return nil, err
	}

	p, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Parameters/Arguments must be an object")
	}

	for k := range p {
		found := false
		for _, a := range allowed {
			if a == k {
				found = true
			}
		}

		if !found {
			return nil, fmt.Errorf("unsupported parameter %s", k)
		}
	}

	for _, name := range allowed {
		if v, exists := p[name]; exists {
			if _, ok := v.(string); !ok {
				return nil, fmt.Errorf("%s must be a string", name)
			}
		}
	}

	return p, nil
}

func (r *run) objectContext(ctx context.Context) context.Context {
	return request.WithMetadata(ctx, request.Metadata{Scope: r.scope, Endpoint: r.exec.Endpoint})
}

func (r *run) readMapItems(ctx context.Context, s *state, input any, e *env) (items []any, source string, err error) {
	rd := s.ItemReader
	if rd == nil {
		v, itemsErr := r.items(s, input, e)
		if itemsErr != nil {
			return nil, "", itemsErr
		}

		items, err = mapInputItems(v)
		if err != nil {
			return nil, "", err
		}

		return items, "STATE_DATA", nil
	}

	defer func() {
		if err != nil {
			err = &stateError{Name: "States.ItemReaderFailed", Cause: err.Error()}
		}
	}()
	if r.s.s3 == nil {
		return nil, "", fmt.Errorf("S3 is unavailable")
	}

	limit := float64(100000000)
	if c := rd.ReaderConfig; c != nil {
		limit, err = mapNumber(e, c.MaxItems, c.MaxItemsPath, "MaxItems", input, limit, 1, limit, true)
		if err != nil {
			return nil, "", err
		}
	}

	p, err := mapParameters(
		s,
		rd.Parameters,
		rd.Arguments,
		input,
		e,
		"Bucket",
		fieldKey,
		"Prefix",
		"VersionId",
		"ExpectedBucketOwner",
	)
	if err != nil {
		return nil, "", err
	}

	bucket, _ := p["Bucket"].(string)
	if bucket == "" {
		return nil, "", fmt.Errorf("bucket is required")
	}

	ctx = r.objectContext(ctx)
	if bucketErr := r.checkMapBucket(ctx, bucket); bucketErr != nil {
		return nil, "", bucketErr
	}

	if rd.Resource == "arn:aws:states:::s3:listObjectsV2" {
		if p[fieldKey] != nil || p["VersionId"] != nil {
			return nil, "", fmt.Errorf("key and VersionId require getObject")
		}

		prefix, _ := p["Prefix"].(string)
		owner, _ := p["ExpectedBucketOwner"].(string)
		var token *string
		for {
			listed, listErr := r.s.s3.ListObjectsV2(ctx, &s3api.ListObjectsV2Request{
				Bucket:              new(bucket),
				Prefix:              new(prefix),
				ContinuationToken:   token,
				ExpectedBucketOwner: optional(owner),
			})
			if listErr != nil {
				return nil, "", listErr
			}

			for _, o := range listed.Contents {
				items = append(items, map[string]any{
					fieldKey: common.Value(o.Key), "Size": common.Value(o.Size), "ETag": common.Value(o.ETag),
					"LastModified": o.LastModified, "StorageClass": common.Value(o.StorageClass),
				})
				if len(items) >= int(limit) {
					return items, "S3://" + bucket, nil
				}
			}

			if !common.Value(listed.IsTruncated) {
				break
			}

			token = listed.NextContinuationToken
		}

		return items, "S3://" + bucket, nil
	}

	if p["Prefix"] != nil {
		return nil, "", fmt.Errorf("prefix requires listObjectsV2")
	}

	key, _ := p[fieldKey].(string)
	if key == "" {
		return nil, "", fmt.Errorf("key is required")
	}

	version, _ := p["VersionId"].(string)
	owner, _ := p["ExpectedBucketOwner"].(string)
	obj, err := r.s.s3.GetObject(ctx, &s3api.GetObjectRequest{
		Bucket: new(bucket), Key: new(key), VersionId: optional(version), ExpectedBucketOwner: optional(owner),
	})
	if err != nil {
		return nil, "", err
	}

	c := rd.ReaderConfig
	switch c.InputType {
	case inputJSON:
		var v any
		if decodeErr := json.Unmarshal(obj.Body, &v); decodeErr != nil {
			return nil, "", decodeErr
		}

		items, err = mapInputItems(v)
		if err != nil {
			return nil, "", err
		}
	case inputJSONL:
		scanner := bufio.NewScanner(bytes.NewReader(obj.Body))
		scanner.Buffer(nil, max(len(obj.Body), 1))
		for scanner.Scan() {
			var item any
			if decodeErr := json.Unmarshal(scanner.Bytes(), &item); decodeErr != nil {
				return nil, "", decodeErr
			}

			items = append(items, item)
			if len(items) >= int(limit) {
				break
			}
		}

		if scanErr := scanner.Err(); scanErr != nil {
			return nil, "", scanErr
		}
	case inputCSV:
		reader := csv.NewReader(bytes.NewReader(obj.Body))
		reader.FieldsPerRecord = -1
		delimiters := map[string]rune{"": ',', "COMMA": ',', "PIPE": '|', "SEMICOLON": ';', "SPACE": ' ', "TAB": '\t'}
		reader.Comma = delimiters[c.CSVDelimiter]
		headers := c.CSVHeaders
		if c.CSVHeaderLocation == "FIRST_ROW" {
			headers, err = reader.Read()
			if err != nil {
				return nil, "", err
			}
		}

		if len(strings.Join(headers, "")) > 10<<10 {
			return nil, "", fmt.Errorf("CSV headers exceed 10 KiB")
		}

		for len(items) < int(limit) {
			row, readErr := reader.Read()
			if readErr == io.EOF {
				break
			}

			if readErr != nil {
				return nil, "", readErr
			}

			item := map[string]any{}
			for i, header := range headers {
				item[header] = ""
				if i < len(row) {
					item[header] = row[i]
				}
			}

			items = append(items, item)
		}
	default:
		return nil, "", fmt.Errorf("unsupported InputType %s", c.InputType)
	}

	if len(items) > int(limit) {
		items = items[:int(limit)]
	}

	if items == nil {
		items = []any{}
	}

	return items, "S3://" + bucket + "/" + key, nil
}

func (r *run) writeMapResults(ctx context.Context, s *state, input any, e *env, mr *mapRun,
	jobs []mapJob,
	results []*execution,
) (any, error) {
	w := s.ResultWriter
	// A stopped Map Run still exports completed and pending child inputs.
	writerEnv := *e
	writerEnv.ctx = context.WithoutCancel(e.ctx)
	e = new(writerEnv)
	ctx = context.WithoutCancel(ctx)
	transformation, outputType := transformNone, inputJSON
	if w.Resource == "" {
		transformation = "COMPACT"
	}

	if c := w.WriterConfig; c != nil {
		if c.Transformation != "" {
			transformation = c.Transformation
		}

		if c.OutputType != "" {
			outputType = c.OutputType
		}
	}

	if w.Resource == "" {
		values := compactMapResults(results, transformation)
		if outputType == inputJSONL {
			return string(mapResultBytes(values, outputType)), nil
		}

		return values, nil
	}

	p, err := mapParameters(s, w.Parameters, w.Arguments, input, e, "Bucket", "Prefix")
	if err != nil {
		return nil, err
	}

	bucket, _ := p["Bucket"].(string)
	prefix, _ := p["Prefix"].(string)
	if bucket == "" || r.s.s3 == nil {
		return nil, fmt.Errorf("ResultWriter requires an available S3 Bucket")
	}

	if bucketErr := r.checkMapBucket(r.objectContext(ctx), bucket); bucketErr != nil {
		return nil, bucketErr
	}

	_, id, _ := strings.Cut(mr.ARN[strings.LastIndex(mr.ARN, ":mapRun:")+8:], ":")
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix != "" {
		prefix += "/"
	}

	prefix += id + "/"
	files := map[string]any{statusSucceeded: []any{}, statusFailed: []any{}, "PENDING": []any{}}
	for _, child := range results {
		if child == nil {
			continue
		}

		category := statusFailed
		if child.Status == statusSucceeded {
			category = statusSucceeded
		}

		values := compactMapResults([]*execution{child}, transformation)
		files[category] = append(files[category].([]any), values...)
	}

	// Pending inputs are exported even when no child execution was started.
	// https://docs.aws.amazon.com/step-functions/latest/dg/input-output-resultwriter.html
	for i, child := range results {
		if child == nil {
			files["PENDING"] = append(files["PENDING"].([]any), map[string]any{fieldInput: *text(jobs[i].Input)})
		}
	}

	resultFiles := map[string]any{}
	ctx = r.objectContext(context.WithoutCancel(ctx))
	for _, status := range []string{statusSucceeded, statusFailed, "PENDING"} {
		values := files[status].([]any)
		if len(values) == 0 {
			continue
		}

		shard, start := 0, 0
		for start < len(values) {
			end, size := start, int64(2)
			for end < len(values) {
				encoded, marshalErr := json.Marshal(values[end])
				if marshalErr != nil {
					return nil, marshalErr
				}

				nextSize := size + int64(len(encoded)) + 1
				if nextSize > 5<<30 {
					if end == start {
						return nil, fmt.Errorf("a result exceeds the S3 result file size limit")
					}

					break
				}

				size = nextSize
				end++
			}

			data := mapResultBytes(values[start:end], outputType)
			key := fmt.Sprintf("%s%s_%d.json", prefix, status, shard)
			if _, writeErr := r.s.s3.PutObject(ctx, &s3api.PutObjectRequest{
				Bucket: new(bucket), Key: new(key), Body: data, ContentType: new("application/json"),
			}); writeErr != nil {
				return nil, writeErr
			}

			entries, _ := resultFiles[status].([]any)
			resultFiles[status] = append(entries, map[string]any{fieldKey: key, "Size": len(data)})
			start, shard = end, shard+1
		}
	}

	manifest := map[string]any{
		"DestinationBucket": bucket, "MapRunArn": mr.ARN, "ResultFiles": resultFiles,
	}

	data, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}

	key := prefix + "manifest.json"
	if _, writeErr := r.s.s3.PutObject(ctx, &s3api.PutObjectRequest{
		Bucket: new(bucket), Key: new(key), Body: data, ContentType: new("application/json"),
	}); writeErr != nil {
		return nil, writeErr
	}

	if _, err := r.s.changeMapRun(ctx, mr.ARN, func(m *mapRun) {
		m.Items.ResultsWritten = m.Items.Succeeded + m.Items.Failed + m.Items.TimedOut + m.Items.Aborted
		m.Executions.ResultsWritten = m.Executions.Succeeded + m.Executions.Failed +
			m.Executions.TimedOut + m.Executions.Aborted
	}); err != nil {
		return nil, err
	}

	return map[string]any{
		"MapRunArn":           mr.ARN,
		"ResultWriterDetails": map[string]any{"Bucket": bucket, fieldKey: key},
	}, nil
}

func mapResultBytes(values []any, outputType string) []byte {
	if outputType != inputJSONL {
		data, _ := json.Marshal(values)
		return data
	}

	var b bytes.Buffer
	for _, value := range values {
		data, _ := json.Marshal(value)
		b.Write(data)
		b.WriteByte('\n')
	}

	return b.Bytes()
}

type mapObjectItem struct {
	Value any
	Key   string
}

func mapInputItems(input any) ([]any, error) {
	switch v := input.(type) {
	case []any:
		return v, nil
	case map[string]any:
		items := make([]any, 0, len(v))
		for _, key := range slices.Sorted(maps.Keys(v)) {
			items = append(items, mapObjectItem{Key: key, Value: v[key]})
		}

		return items, nil
	default:
		return nil, &stateError{Name: errRuntime, Cause: "Map items must be an array or object"}
	}
}

func (r *run) checkMapBucket(ctx context.Context, bucket string) error {
	location, err := r.s.s3.GetBucketLocation(ctx, &s3api.GetBucketLocationRequest{
		Bucket: new(bucket), ExpectedBucketOwner: new(r.scope.AccountID),
	})
	if err != nil {
		return err
	}

	region := common.Value(location.LocationConstraint)
	if region == "" {
		region = "us-east-1"
	}

	if region != r.scope.Region {
		return fmt.Errorf("map S3 bucket must be in the state machine's region")
	}

	return nil
}
