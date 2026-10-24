package tabular

import (
	"reflect"
	"testing"
)

func TestClassifyText(t *testing.T) {
	cases := []struct {
		name, body string
		options    *ClassifyOptions
		columns    []Column
		header     bool
	}{
		{
			"csv",
			"id,score,enabled,at,name\n1,1.5,true,2025-01-02 03:04:05,alice\n2,2,false,2025-01-03 04:05:06,bob\n",
			&ClassifyOptions{},
			[]Column{
				{"id", "bigint"},
				{"score", "double"},
				{"enabled", "boolean"},
				{"at", "timestamp"},
				{"name", "string"},
			},
			true,
		},
		{"tsv", "1\talpha\n2\tbeta\n", &ClassifyOptions{}, []Column{{"col0", "bigint"}, {"col1", "string"}}, false},
		{"pipe", "id|name\n1|alice\n", &ClassifyOptions{}, []Column{{"id", "bigint"}, {"name", "string"}}, true},
		{
			"header absent",
			"name,value\nalice,hello\n",
			&ClassifyOptions{Kind: CSV, Delimiter: ",", ContainsHeader: "ABSENT"},
			[]Column{{"col0", "string"}, {"col1", "string"}},
			false,
		},
		{
			"custom",
			"a;b\n1;false\n",
			&ClassifyOptions{
				Kind:           CSV,
				Delimiter:      ";",
				ContainsHeader: "PRESENT",
				Header:         []string{"identifier", "flag"},
				Types:          []string{"string", "boolean"},
			},
			[]Column{{"identifier", "string"}, {"flag", "boolean"}},
			true,
		},
		{
			"json",
			"{\"id\":1,\"nested\":{\"flag\":true},\"values\":[1,2.5]}\n",
			&ClassifyOptions{},
			[]Column{{"id", "bigint"}, {"nested", "struct<flag:boolean>"}, {"values", "array<double>"}},
			false,
		},
		{
			"jsonpath",
			"{\"records\":[{\"id\":1},{\"id\":2}]}\n",
			&ClassifyOptions{Kind: JSON, JSONPath: "$.records[*]"},
			[]Column{{"id", "bigint"}},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Classify([]byte(tc.body), tc.options)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(c.Columns, tc.columns) || c.Header != tc.header {
				t.Fatalf("classification %+v", c)
			}
		})
	}
	for _, body := range []string{"not tabular text", "\x00\x01\x02", "{invalid}"} {
		if _, err := Classify([]byte(body), &ClassifyOptions{}); err == nil {
			t.Fatalf("classified %q", body)
		}
	}
}
