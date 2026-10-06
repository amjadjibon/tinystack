package athena

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strconv"
	"strings"

	glueapi "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/common"
)

// Row formats TinyStack reads.
const (
	formatDelimited = "delimited"
	formatCSV       = "csv"
	formatJSON      = "json"
)

// rowFormat is how a table's objects hold its rows.
type rowFormat struct {
	kind  string
	delim string
	// null is the field that means NULL in delimited text.
	null       string
	skipHeader int
	// lenient skips JSON lines that are not objects.
	lenient bool
}

// formatOf reads a table's row format from its SerDe and parameters.
func formatOf(t *glueapi.Table) (*rowFormat, error) {
	sd := common.OrElse(t.StorageDescriptor, &glueapi.StorageDescriptor{})
	serde := common.OrElse(sd.SerdeInfo, &glueapi.SerDeInfo{})
	lib := strings.ToLower(common.Value(serde.SerializationLibrary))
	input := strings.ToLower(common.Value(sd.InputFormat))
	params := serde.Parameters
	f := &rowFormat{kind: formatDelimited, delim: "\x01", null: `\N`}
	if n, err := strconv.Atoi(t.Parameters["skip.header.line.count"]); err == nil {
		f.skipHeader = n
	}

	classification := strings.ToLower(t.Parameters["classification"])
	switch {
	case strings.Contains(lib, "parquet") || strings.Contains(lib, "orc") || strings.Contains(lib, "avro") ||
		strings.Contains(input, "parquet") || strings.Contains(input, "orc") || strings.Contains(input, "avro") ||
		strings.Contains(lib, "iceberg") || classification == "parquet":
		return nil, fmt.Errorf("NOT_SUPPORTED: TinyStack reads text (CSV and delimited) and JSON tables, not %s",
			common.Value(common.OrElse(serde.SerializationLibrary, sd.InputFormat)))
	case strings.Contains(lib, "jsonserde") || lib == "" && classification == formatJSON:
		f.kind = formatJSON
		f.lenient = strings.EqualFold(params["ignore.malformed.json"], "true")
	case strings.Contains(lib, "opencsvserde"):
		f.kind, f.delim = formatCSV, ","
		if sep := params["separatorChar"]; sep != "" {
			f.delim = sep[:1]
		}

		if q := params["quoteChar"]; q != "" && q != `"` {
			return nil, fmt.Errorf(`TinyStack reads CSV quoted with ", not %s`, q)
		}
	case lib == "" && classification == formatCSV:
		f.delim = ","
	}

	if f.kind == formatDelimited {
		for _, key := range []string{keyFieldDelim, "serialization.format"} {
			if d := params[key]; d != "" && (key == keyFieldDelim || len(d) == 1 && d[0] > '9') {
				f.delim = d
				break
			}
		}

		if n, ok := params["serialization.null.format"]; ok {
			f.null = n
		}
	}

	return f, nil
}

// records splits an object's bytes into rows of fields, nil for NULL, as
// columns names the table's columns.
func (f *rowFormat) records(body []byte, columns []string) ([][]*string, error) {
	var out [][]*string
	switch f.kind {
	case formatCSV:
		r := csv.NewReader(bytes.NewReader(body))
		r.Comma, r.LazyQuotes, r.FieldsPerRecord = rune(f.delim[0]), true, -1
		for {
			fields, err := r.Read()
			if errors.Is(err, io.EOF) {
				break
			}

			if err != nil {
				return nil, fmt.Errorf("HIVE_BAD_DATA: %w", err)
			}

			row := make([]*string, len(fields))
			for i := range fields {
				row[i] = &fields[i]
			}

			out = append(out, row)
		}
	case formatJSON:
		scanner := bufio.NewScanner(bytes.NewReader(body))
		scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}

			row, err := jsonRow(line, columns)
			if err != nil && f.lenient {
				continue
			}

			if err != nil {
				return nil, err
			}

			out = append(out, row)
		}

		if err := scanner.Err(); err != nil {
			return nil, err
		}
	default:
		for line := range strings.SplitSeq(string(body), "\n") {
			line = strings.TrimSuffix(line, "\r")
			if line == "" {
				continue
			}

			fields := strings.Split(line, f.delim)
			row := make([]*string, len(fields))
			for i := range fields {
				if fields[i] != f.null {
					row[i] = &fields[i]
				}
			}

			out = append(out, row)
		}
	}

	if f.skipHeader > 0 && f.kind != formatJSON {
		out = out[min(f.skipHeader, len(out)):]
	}

	return out, nil
}

// jsonRow reads a JSON object's members into a row, matching column names
// without case; nested values stay JSON text.
func jsonRow(line string, columns []string) ([]*string, error) {
	d := json.NewDecoder(strings.NewReader(line))
	d.UseNumber()
	var object map[string]any
	if err := d.Decode(&object); err != nil || object == nil {
		return nil, fmt.Errorf("HIVE_CURSOR_ERROR: Row is not a valid JSON Object: %s", line)
	}

	members := make(map[string]any, len(object))
	for k, v := range object {
		members[strings.ToLower(k)] = v
	}

	row := make([]*string, len(columns))
	for i, c := range columns {
		switch v := members[c].(type) {
		case nil:
		case string:
			row[i] = &v
		case json.Number:
			row[i] = new(v.String())
		case bool:
			row[i] = new(strconv.FormatBool(v))
		default:
			b, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}

			row[i] = new(string(b))
		}
	}

	return row, nil
}

// location splits an s3:// location into its bucket and its key prefix,
// which ends with / as a directory does.
func location(uri string) (bucket, prefix string, err error) {
	rest, ok := strings.CutPrefix(uri, "s3://")
	if !ok {
		rest, ok = strings.CutPrefix(uri, "s3a://")
	}

	bucket, prefix, _ = strings.Cut(rest, "/")
	if !ok || bucket == "" {
		return "", "", fmt.Errorf("the location %s is not an s3:// URI", uri)
	}

	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	return bucket, prefix, nil
}

// source is where a table, or one of its partitions, keeps its objects.
type source struct {
	bucket, prefix string
	// values are the partition's, or nil to read them from each object's
	// key=value directories.
	values []string
}

// sources lists where a table's rows are: its location, or its registered
// partitions' locations, or, for a table with partition projection enabled,
// its location with the partition values read from object keys. A
// partitioned table without either has no rows, as in Athena.
func (s *Service) sources(ctx context.Context, t *glueapi.Table) ([]source, error) {
	sd := common.OrElse(t.StorageDescriptor, &glueapi.StorageDescriptor{})
	bucket, prefix, err := location(common.Value(sd.Location))
	if err != nil {
		return nil, err
	}

	if len(t.PartitionKeys) == 0 {
		return []source{{bucket: bucket, prefix: prefix}}, nil
	}

	var out []source
	for token := (*string)(nil); ; {
		p, err := s.catalog.GetPartitions(ctx, &glueapi.GetPartitionsRequest{
			DatabaseName: t.DatabaseName, TableName: t.Name, NextToken: token,
		})
		if err != nil {
			return nil, err
		}

		for _, part := range p.Partitions {
			loc := common.Value(common.OrElse(part.StorageDescriptor, &glueapi.StorageDescriptor{}).Location)
			if loc == "" {
				loc = "s3://" + bucket + "/" + prefix + partitionPath(t.PartitionKeys, part.Values)
			}

			b, pre, locErr := location(loc)
			if locErr != nil {
				return nil, locErr
			}

			out = append(out, source{bucket: b, prefix: pre, values: part.Values})
		}

		if token = p.NextToken; token == nil {
			break
		}
	}

	if len(out) == 0 && strings.EqualFold(t.Parameters["projection.enabled"], "true") {
		out = append(out, source{bucket: bucket, prefix: prefix})
	}

	return out, nil
}

// partitionPath is the Hive directory path of a partition: k1=v1/k2=v2/.
func partitionPath(keys []glueapi.Column, values []string) string {
	var b strings.Builder
	for i, k := range keys {
		b.WriteString(common.Value(k.Name) + "=" + url.PathEscape(values[i]) + "/")
	}

	return b.String()
}

// pathValues reads partition values from a key's key=value directories
// below prefix, reporting false when one is missing.
func pathValues(keys []glueapi.Column, prefix, key string) ([]string, bool) {
	found := map[string]string{}
	for _, dir := range strings.Split(strings.TrimPrefix(key, prefix), "/") {
		if k, v, ok := strings.Cut(dir, "="); ok {
			unescaped, err := url.PathUnescape(v)
			if err != nil {
				unescaped = v
			}

			found[strings.ToLower(k)] = unescaped
		}
	}

	values := make([]string, len(keys))
	for i, k := range keys {
		v, ok := found[strings.ToLower(common.Value(k.Name))]
		if !ok {
			return nil, false
		}

		values[i] = v
	}

	return values, true
}

// object is a data object of a table, with its partition's values.
type object struct {
	bucket, key string
	values      []string
	size        int64
}

// objects lists a source's data objects, skipping the hidden ones (names
// starting with _ or .) and directory markers.
func (s *Service) objectsOf(ctx context.Context, t *glueapi.Table, src source) ([]object, error) {
	var out []object
	for token := (*string)(nil); ; {
		list, err := s.objects.ListObjectsV2(ctx, &s3api.ListObjectsV2Request{
			Bucket: new(src.bucket), Prefix: new(src.prefix), ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}

		for _, o := range list.Contents {
			key := common.Value(o.Key)
			base := path.Base(key)
			if strings.HasSuffix(key, "/") || strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".") ||
				strings.HasSuffix(key, "_$folder$") {
				continue
			}

			values := src.values
			if values == nil && len(t.PartitionKeys) > 0 {
				var ok bool
				if values, ok = pathValues(t.PartitionKeys, src.prefix, key); !ok {
					continue
				}
			}

			out = append(out, object{bucket: src.bucket, key: key, values: values, size: common.Value(o.Size)})
		}

		if !common.Value(list.IsTruncated) {
			return out, nil
		}

		token = list.NextContinuationToken
	}
}

// read returns an object's bytes, decompressing .gz objects.
func (s *Service) read(ctx context.Context, o object) ([]byte, error) {
	body, err := s.objects.Object(ctx, o.bucket, o.key, "")
	if err != nil || !strings.HasSuffix(o.key, ".gz") {
		return body, err
	}

	r, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("HIVE_CURSOR_ERROR: %s is not gzip: %w", o.key, err)
	}

	return io.ReadAll(io.LimitReader(r, 1<<30))
}
