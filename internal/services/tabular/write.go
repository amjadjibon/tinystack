package tabular

import (
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func (f *Format) writeText(w io.Writer, columns []Column, rows [][]*string, compression string) error {
	if strings.EqualFold(compression, "GZIP") || compression == "" {
		z := gzip.NewWriter(w)
		if err := f.writeText(z, columns, rows, "NONE"); err != nil {
			return err
		}

		return z.Close()
	}

	if !strings.EqualFold(compression, "NONE") && !strings.EqualFold(compression, "UNCOMPRESSED") {
		return fmt.Errorf("NOT_SUPPORTED: text compression %s", compression)
	}

	encoder := json.NewEncoder(w)
	writer := csv.NewWriter(w)
	if f.Kind == CSV {
		writer.Comma = rune(f.Delimiter[0])
	}

	for _, row := range rows {
		switch f.Kind {
		case JSON:
			obj := map[string]any{}
			for i, c := range columns {
				if i < len(row) && row[i] != nil {
					v, err := jsonValue(*row[i], c.Type)
					if err != nil {
						return err
					}

					obj[c.Name] = v
				} else {
					obj[c.Name] = nil
				}
			}

			if err := encoder.Encode(obj); err != nil {
				return err
			}
		case CSV:
			r := make([]string, len(columns))
			for i := range r {
				if i < len(row) && row[i] != nil {
					r[i] = *row[i]
				}
			}

			if err := writer.Write(r); err != nil {
				return err
			}
		default:
			fields := make([]string, len(columns))
			for i := range fields {
				fields[i] = f.Null
				if i < len(row) && row[i] != nil {
					fields[i] = *row[i]
				}
			}

			if _, err := io.WriteString(w, strings.Join(fields, f.Delimiter)+"\n"); err != nil {
				return err
			}
		}
	}

	writer.Flush()
	return writer.Error()
}

func jsonValue(text, kind string) (any, error) {
	base := strings.ToLower(strings.TrimSpace(kind))
	if i := strings.IndexAny(base, "(<"); i >= 0 {
		base = base[:i]
	}

	switch base {
	case hiveBoolean,
		hiveTinyint,
		hiveSmallint,
		hiveInt,
		"integer",
		hiveBigint,
		hiveFloat,
		hiveDouble,
		"real",
		"decimal",
		"array",
		"map",
		"struct":
		var v any
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.UseNumber()
		if err := decoder.Decode(&v); err != nil {
			return nil, fmt.Errorf("invalid %s value: %w", kind, err)
		}

		return v, nil
	default:
		return text, nil
	}
}
