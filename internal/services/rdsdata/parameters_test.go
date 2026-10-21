package rdsdata

import (
	"reflect"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/rdsdata"
)

func TestBindSQLSyntax(t *testing.T) {
	parameters := []api.SqlParameter{{Name: new("value"), Value: &api.Field{LongValue: new(int64(7))}}}
	for _, driver := range []string{driverPostgres, driverMySQL} {
		query := `SELECT ':value', "a:value", :value, :value /* :ignored /* nested */ */ -- :ignored
`
		expected := `SELECT ':value', "a:value", ?, ? /* :ignored /* nested */ */ -- :ignored
`
		if driver == driverPostgres {
			expected = `SELECT ':value', "a:value", CAST($1 AS BIGINT), CAST($2 AS BIGINT)` +
				` /* :ignored /* nested */ */ -- :ignored
`
		}

		statement, args, err := bind(query, driver, parameters)
		if err != nil || statement != expected || !reflect.DeepEqual(args, []any{int64(7), int64(7)}) {
			t.Fatal(statement, args, err)
		}
	}

	query := `SELECT $$:ignored$$, $tag$:ignored$tag$, :value::bigint`
	statement, _, err := bind(query, driverPostgres, parameters)
	if err != nil || statement != `SELECT $$:ignored$$, $tag$:ignored$tag$, CAST($1 AS BIGINT)::bigint` {
		t.Fatal(statement, err)
	}
}

func TestParameterValidation(t *testing.T) {
	for _, parameters := range [][]api.SqlParameter{
		{{Name: new("value"), Value: &api.Field{LongValue: new(int64(1)), StringValue: new("bad")}}},
		{{Name: new("value"), Value: &api.Field{IsNull: new(false)}}},
		{{Name: new("value"), Value: &api.Field{StringValue: new("x")}, TypeHint: new("UNSUPPORTED")}},
		{
			{Name: new("value"), Value: &api.Field{StringValue: new("x")}},
			{Name: new("value"), Value: &api.Field{StringValue: new("x")}},
		},
		nil,
	} {
		if _, _, err := bind("SELECT :value", driverPostgres, parameters); err == nil {
			t.Fatalf("accepted %+v", parameters)
		}
	}
}

func TestResultFields(t *testing.T) {
	cases := []struct {
		value any
		field api.Field
		typ   string
	}{
		{value: nil, typ: "TEXT", field: api.Field{IsNull: new(true)}},
		{value: []byte("12.50"), typ: "NUMERIC", field: api.Field{StringValue: new("12.50")}},
		{value: []byte{0, 1, 2}, typ: "BYTEA", field: api.Field{BlobValue: new([]byte{0, 1, 2})}},
		{value: []byte("42"), typ: "BIGINT", field: api.Field{LongValue: new(int64(42))}},
		{value: true, typ: "BOOL", field: api.Field{BooleanValue: new(true)}},
		{value: 1.5, typ: "FLOAT8", field: api.Field{DoubleValue: new(1.5)}},
	}

	for _, tc := range cases {
		field, err := resultField(tc.value, tc.typ)
		if err != nil || !reflect.DeepEqual(field, tc.field) {
			t.Fatal(field, err)
		}
	}
}
