package tabular

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	Delimited = "delimited"
	CSV       = "csv"
	JSON      = "json"
	Parquet   = "parquet"
)

// Format describes a row encoding independently of any catalog or object store.
type Format struct {
	Kind       string
	Delimiter  string
	Null       string
	SkipHeader int
	Lenient    bool
}

// records splits an object's bytes into rows of fields, nil for NULL, as
// columns names the table's columns.
func (f *Format) Records(body []byte, columns []string) ([][]*string, error) {
	if f.Kind == Parquet {
		return readParquet(body, columns)
	}
	var out [][]*string
	switch f.Kind {
	case CSV:
		r := csv.NewReader(bytes.NewReader(body))
		r.Comma, r.LazyQuotes, r.FieldsPerRecord = rune(f.Delimiter[0]), true, -1
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
	case JSON:
		scanner := bufio.NewScanner(bytes.NewReader(body))
		scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}

			row, err := jsonRow(line, columns)
			if err != nil && f.Lenient {
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

			fields := strings.Split(line, f.Delimiter)
			row := make([]*string, len(fields))
			for i := range fields {
				if fields[i] != f.Null {
					row[i] = &fields[i]
				}
			}

			out = append(out, row)
		}
	}

	if f.SkipHeader > 0 && f.Kind != JSON {
		out = out[min(f.SkipHeader, len(out)):]
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
