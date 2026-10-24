package tabular

import (
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ClassifyOptions describes a custom text classifier. Empty Kind selects built-ins.
type ClassifyOptions struct {
	Kind, Delimiter, Quote, ContainsHeader, JSONPath string
	Header, Types                                    []string
	DisableTrimming, SingleColumn                    bool
}

// Classification is independent of catalog records and object storage.
type Classification struct {
	Kind, Delimiter, Quote, Compression string
	Columns                             []Column
	Records                             int64
	Header                              bool
}

var jsonPathPattern = regexp.MustCompile(`^\$(\.[A-Za-z_][A-Za-z0-9_]*)*(\[\*\])?$`)

// ValidJSONPath reports the JSONPath subset supported by classification.
func ValidJSONPath(p string) bool { return jsonPathPattern.MatchString(p) }

// ValidCrawlType reports the supported CSV override types.
func ValidCrawlType(t string) bool {
	return slices.Contains([]string{hiveBigint, hiveDouble, hiveBoolean, hiveString, hiveTimestamp}, strings.ToLower(t))
}

// Classify infers a supported object's schema. An error means no confident classification.
func Classify(body []byte, o *ClassifyOptions) (*Classification, error) {
	compression := "none"
	if len(body) > 2 && body[0] == 0x1f && body[1] == 0x8b {
		r, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}

		body, err = io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			return nil, err
		}

		compression = "gzip"
	}

	c, err := classify(body, o)
	if err != nil {
		return nil, err
	}

	c.Compression = compression
	return c, nil
}

func classify(body []byte, o *ClassifyOptions) (*Classification, error) {
	if o.Kind == "" && bytes.HasPrefix(body, []byte("PAR1")) {
		cols, err := Infer(body)
		if err != nil {
			return nil, err
		}

		names := make([]string, len(cols))
		for i, c := range cols {
			names[i] = c.Name
		}

		rows, err := (&Format{Kind: Parquet}).Records(body, names)
		return &Classification{Columns: cols, Kind: Parquet, Records: int64(len(rows))}, err
	}

	if o.Kind == JSON ||
		o.Kind == "" &&
			(bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) || bytes.HasPrefix(bytes.TrimSpace(body), []byte("["))) {
		return classifyJSON(body, o.JSONPath)
	}

	if o.Delimiter != "" {
		return classifyCSV(body, o)
	}

	for _, d := range []string{",", "\t", "|", ";"} {
		try := *o
		try.Delimiter = d
		if c, err := classifyCSV(body, &try); err == nil {
			return c, nil
		}
	}

	return nil, fmt.Errorf("unclassified text")
}

func scalarType(v string) string {
	if v == "" {
		return ""
	}

	if _, err := strconv.ParseInt(v, 10, 64); err == nil {
		return hiveBigint
	}

	if _, err := strconv.ParseFloat(v, 64); err == nil {
		return hiveDouble
	}

	if strings.EqualFold(v, "true") || strings.EqualFold(v, "false") {
		return "boolean"
	}

	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999"} {
		if _, err := time.Parse(layout, v); err == nil {
			return "timestamp"
		}
	}

	return hiveString
}

func mergeType(a, b string) string {
	if a == "" {
		return b
	}

	if b == "" || a == b {
		return a
	}

	if a == hiveBigint && b == hiveDouble || a == hiveDouble && b == hiveBigint {
		return hiveDouble
	}

	return hiveString
}

func classifyCSV(body []byte, o *ClassifyOptions) (*Classification, error) {
	if bytes.IndexByte(body, 0) >= 0 {
		return nil, fmt.Errorf("binary text")
	}

	quote := o.Quote
	if quote == "" {
		quote = `"`
	}

	// encoding/csv implements double-quote escaping. Normalize custom quote symbols first.
	if quote != `"` {
		body = bytes.ReplaceAll(body, []byte(quote), []byte(`"`))
	}

	r := csv.NewReader(bytes.NewReader(body))
	r.Comma, _ = utf8.DecodeRuneInString(o.Delimiter)
	rows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}

	if len(rows) == 0 || len(rows[0]) < 2 && !o.SingleColumn {
		return nil, fmt.Errorf("insufficient CSV columns")
	}

	width := len(rows[0])
	header := o.ContainsHeader == "PRESENT"
	if o.ContainsHeader == "" || o.ContainsHeader == "UNKNOWN" {
		// A header is confident when all first-row fields are names and later rows contain typed data.
		header = len(rows) > 1
		typed := false
		seen := map[string]bool{}
		for i, v := range rows[0] {
			v = strings.TrimSpace(v)
			if scalarType(v) != hiveString || seen[v] || v == "" {
				header = false
			}

			seen[v] = true
			for _, row := range rows[1:] {
				if scalarType(strings.TrimSpace(row[i])) != hiveString {
					typed = true
				}
			}
		}

		header = header && typed
	}

	cols := make([]Column, width)
	start := 0
	if header {
		start = 1
	}

	for i := range width {
		n := fmt.Sprintf("col%d", i)
		if header {
			n = strings.ToLower(strings.TrimSpace(rows[0][i]))
		}

		if i < len(o.Header) {
			n = o.Header[i]
		}

		typ := ""
		for _, row := range rows[start:] {
			v := row[i]
			if !o.DisableTrimming {
				v = strings.TrimSpace(v)
			}

			typ = mergeType(typ, scalarType(v))
		}

		if typ == "" {
			typ = hiveString
		}

		if i < len(o.Types) {
			typ = strings.ToLower(o.Types[i])
		}

		cols[i] = Column{n, typ}
	}

	if len(o.Types) > 0 && len(o.Types) != width {
		return nil, fmt.Errorf("CustomDatatypes count differs from columns")
	}

	return &Classification{
		Columns:   cols,
		Kind:      CSV,
		Delimiter: o.Delimiter,
		Quote:     quote,
		Header:    header,
		Records:   int64(len(rows) - start),
	}, nil
}

func classifyJSON(body []byte, p string) (*Classification, error) {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	var objects []map[string]any
	for {
		var v any
		err := d.Decode(&v)
		if err == io.EOF {
			break
		}

		if err != nil {
			return nil, err
		}

		path := strings.TrimSuffix(strings.TrimPrefix(p, "$"), "[*]")
		for _, k := range strings.Split(strings.TrimPrefix(path, "."), ".") {
			if k == "" {
				continue
			}

			m, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("JsonPath does not select objects")
			}

			v = m[k]
		}

		values := []any{v}
		if a, ok := v.([]any); ok {
			values = a
		}

		for _, v := range values {
			m, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("JSON records must be objects")
			}

			objects = append(objects, m)
		}
	}

	if len(objects) == 0 {
		return nil, fmt.Errorf("no JSON objects")
	}

	types := map[string]string{}
	for _, m := range objects {
		for k, v := range m {
			types[k] = mergeType(types[k], jsonType(v))
		}
	}

	keys := make([]string, 0, len(types))
	for k := range types {
		keys = append(keys, k)
	}

	slices.Sort(keys)
	c := &Classification{Kind: JSON, Records: int64(len(objects))}
	for _, k := range keys {
		t := types[k]
		if t == "" {
			t = hiveString
		}

		c.Columns = append(c.Columns, Column{strings.ToLower(k), t})
	}

	return c, nil
}

func jsonType(v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case bool:
		return "boolean"
	case json.Number:
		return scalarType(v.String())
	case string:
		return hiveString
	case []any:
		t := ""
		for _, x := range v {
			t = mergeType(t, jsonType(x))
		}

		if t == "" {
			t = hiveString
		}

		return "array<" + t + ">"
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}

		slices.Sort(keys)
		fields := make([]string, 0, len(keys))
		for _, k := range keys {
			t := jsonType(v[k])
			if t == "" {
				t = hiveString
			}

			fields = append(fields, strings.ToLower(k)+":"+t)
		}

		return "struct<" + strings.Join(fields, ",") + ">"
	}

	return hiveString
}
