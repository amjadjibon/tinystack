package tabular

import (
	"bytes"
	"reflect"
	"testing"
)

func TestParquetRoundTrip(t *testing.T) {
	columns := []Column{
		{"s", "string"},
		{"v", "varchar(20)"},
		{"c", "char(4)"},
		{"b", "boolean"},
		{"tiny", "tinyint"},
		{"small", "smallint"},
		{"i", "int"},
		{"big", "bigint"},
		{"f", "float"},
		{"d", "double"},
		{"dec", "decimal(12,2)"},
		{"precise", "decimal(38,4)"},
		{"day", "date"},
		{"ts", "timestamp"},
		{"bin", "binary"},
		{"a", "array<int>"},
		{"m", "map<string,int>"},
		{"nested", "struct<a:int,b:string>"},
	}

	values := []string{
		"hello",
		"varchar",
		"char",
		"true",
		"12",
		"1234",
		"123456",
		"12345678900",
		"1.5",
		"2.25",
		"1234.56",
		"-123456789012345678901234567890.1234",
		"2026-01-02",
		"2026-01-02 03:04:05.123456",
		"bytes",
		"[1,2,null]",
		`{"a":1,"b":2}`,
		`{"a":1,"b":"two"}`,
	}

	row := make([]*string, len(values))
	names := make([]string, len(values))
	for i, v := range values {
		row[i] = new(v)
		names[i] = columns[i].Name
	}

	for _, compression := range []string{"NONE", "SNAPPY", "GZIP", "ZSTD"} {
		t.Run(compression, func(t *testing.T) {
			f := &Format{Kind: Parquet}
			var b bytes.Buffer
			if err := f.Write(&b, columns, [][]*string{row, make([]*string, len(row))}, compression); err != nil {
				t.Fatal(err)
			}

			schema, err := Infer(b.Bytes())
			if err != nil {
				t.Fatal(err)
			}

			if len(schema) != len(columns) {
				t.Fatal(schema)
			}

			expected := map[string]string{}
			for _, c := range columns {
				expected[c.Name] = c.Type
			}
			expected["v"], expected["c"] = "string", "string"
			for _, c := range schema {
				if expected[c.Name] != c.Type {
					t.Errorf("inferred %s: got %s, want %s", c.Name, c.Type, expected[c.Name])
				}
			}

			rows, err := f.Records(b.Bytes(), names)
			if err != nil {
				t.Fatal(err)
			}

			if !reflect.DeepEqual(rows, [][]*string{row, make([]*string, len(row))}) {
				for i, v := range rows[0] {
					if v == nil {
						t.Errorf("%s: unexpected NULL", names[i])
						continue
					}
					if *v != values[i] {
						t.Errorf("%s: got %v want %s", names[i], *v, values[i])
					}
				}

				t.Fatalf("rows: %#v", rows)
			}
		})
	}
}

func TestTextReaders(t *testing.T) {
	for _, tt := range []struct {
		body   string
		want   [][]*string
		format Format
	}{
		{
			format: Format{Kind: Delimited, Delimiter: ",", Null: `\N`, SkipHeader: 1},
			body:   "a,b\n1,\\N\n", want: [][]*string{{new("1"), nil}},
		},
		{format: Format{Kind: CSV, Delimiter: ","}, body: "\"a,b\",c\n", want: [][]*string{{new("a,b"), new("c")}}},
		{format: Format{Kind: JSON}, body: `{"a":1,"b":null}`, want: [][]*string{{new("1"), nil}}},
	} {
		rows, err := tt.format.Records([]byte(tt.body), []string{"a", "b"})
		if err != nil {
			t.Fatal(err)
		}

		if !reflect.DeepEqual(rows, tt.want) {
			t.Fatalf("got %v", rows)
		}
	}
}

func TestParquetProjection(t *testing.T) {
	f := &Format{Kind: Parquet}
	var body bytes.Buffer
	if err := f.Write(
		&body,
		[]Column{{"first", "int"}, {"second", "string"}},
		[][]*string{{new("1"), new("kept")}},
		"SNAPPY",
	); err != nil {
		t.Fatal(err)
	}

	count := 0
	err := StreamParquet(
		bytes.NewReader(body.Bytes()),
		int64(body.Len()),
		[]string{"second"},
		func(row []*string) error {
			count++
			if len(row) != 1 || row[0] == nil || *row[0] != "kept" {
				t.Fatalf("%v", row)
			}

			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if count != 1 {
		t.Fatal(count)
	}
}

func TestMalformedNestedTypes(t *testing.T) {
	for _, kind := range []string{"array<", "map<string,int", "struct<a:int"} {
		var body bytes.Buffer
		if err := (&Format{Kind: Parquet}).Write(&body, []Column{{Name: "bad", Type: kind}}, nil, "GZIP"); err == nil {
			t.Fatalf("accepted %s", kind)
		}
	}
}
