package dynamodb

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
)

// attributes decodes DynamoDB JSON, which keeps the test cases readable.
func attributes(t *testing.T, s string) map[string]api.AttributeValue {
	t.Helper()
	var out map[string]api.AttributeValue
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatal(err)
	}

	return out
}

const sample = `{
	"PK": {"S": "user#1"},
	"age": {"N": "42"},
	"name": {"S": "Ada Lovelace"},
	"tags": {"SS": ["admin", "ops"]},
	"scores": {"NS": ["1", "2.5"]},
	"list": {"L": [{"S": "a"}, {"N": "7"}, {"M": {"deep": {"BOOL": true}}}]},
	"profile": {"M": {"city": {"S": "London"}, "zip": {"NULL": true}}},
	"blob": {"B": "AAEC"}
}`

const sampleValues = `{
	":s": {"S": "user#1"}, ":age": {"N": "42.0"}, ":low": {"N": "40"}, ":high": {"N": "50"},
	":prefix": {"S": "Ada"}, ":admin": {"S": "admin"}, ":two": {"N": "2.50"}, ":seven": {"N": "7"},
	":type": {"S": "SS"}, ":london": {"S": "London"}, ":three": {"N": "3"}, ":bytes": {"B": "AA=="}
}`

func TestConditions(t *testing.T) {
	it := attributes(t, sample)
	values := attributes(t, sampleValues)
	names := map[string]string{"#n": "name", "#p": "profile"}
	for expr, want := range map[string]bool{
		"PK = :s":                                           true,
		"age = :age":                                        true,
		"age <> :age":                                       false,
		"missing <> :age":                                   true,
		"age BETWEEN :low AND :high":                        true,
		"age between :high and :high":                       false,
		"age > :low AND age < :high":                        true,
		"age < :low OR begins_with(#n, :prefix)":            true,
		"NOT (age >= :low)":                                 false,
		"age IN (:low, :high, :age)":                        true,
		"attribute_exists(#p.city)":                         true,
		"attribute_not_exists(#p.street)":                   true,
		"attribute_type(tags, :type)":                       true,
		"contains(tags, :admin)":                            true,
		"contains(scores, :two)":                            true,
		"contains(list, :seven)":                            true,
		"contains(#n, :prefix)":                             true,
		"begins_with(blob, :bytes)":                         true,
		"#p.city = :london AND list[2].deep = list[2].deep": true,
		"size(tags) < :three AND size(#n) > :low":           false,
		"size(list) = :three":                               true,
		"size(age) = :three":                                false,
		"list[5] = :seven":                                  false,
		"age > :prefix":                                     false,
	} {
		ex, err := newExpressions(names, values)
		if err != nil {
			t.Fatal(err)
		}

		c, err := ex.condition("ConditionExpression", &expr)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}

		if got := c.match(it); got != want {
			t.Errorf("%s = %v, want %v", expr, got, want)
		}
	}
}

func TestExpressionErrors(t *testing.T) {
	values := attributes(t, `{":v": {"S": "x"}, ":n": {"N": "1"}, ":hi": {"N": "9"}}`)
	for expr, want := range map[string]string{
		"":                           "can not be empty",
		"a = ":                       "token: <EOF>",
		"a = :v AND":                 "token: <EOF>",
		"a = :missing":               "attribute value: :missing",
		"#missing = :v":              "attribute name: #missing",
		"a == :v":                    `token: "="`,
		"a = :v)":                    `token: ")"`,
		"nope(a)":                    "Invalid function name",
		"attribute_type(a, :v)":      "Invalid attribute type name",
		"a BETWEEN :hi AND :n":       "upper bound",
		"a = :v ; drop":              "Invalid character",
		"a[x] = :v":                  `token: "x"`,
		"begins_with(a, :v) = :v":    `token: "="`,
		"a = :v OR b = :v OR c = :n": "",
	} {
		ex, err := newExpressions(nil, values)
		if err != nil {
			t.Fatal(err)
		}

		_, err = ex.condition("ConditionExpression", &expr)
		if want == "" {
			if err != nil {
				t.Errorf("%q: %v", expr, err)
			}

			continue
		}

		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", expr, err, want)
		}
	}

	ex, err := newExpressions(map[string]string{"#a": "a", "#b": "b"}, values)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = ex.condition("ConditionExpression", ptr("#a = :v")); err != nil {
		t.Fatal(err)
	}

	if err = ex.finish(); err == nil || !strings.Contains(err.Error(), "ExpressionAttributeNames unused") ||
		!strings.Contains(err.Error(), "#b") {
		t.Fatalf("unused names: %v", err)
	}

	ex, err = newExpressions(nil, values)
	if err != nil {
		t.Fatal(err)
	}

	if err = ex.finish(); err == nil || !strings.Contains(err.Error(), "only be specified when using expressions") {
		t.Fatalf("values without expressions: %v", err)
	}

	if _, err = newExpressions(map[string]string{"a": "a"}, nil); err == nil {
		t.Fatal("name placeholder without # accepted")
	}

	if _, err = newExpressions(nil, map[string]api.AttributeValue{":v": {}}); err == nil {
		t.Fatal("empty attribute value accepted")
	}
}

func TestProjection(t *testing.T) {
	it := attributes(t, sample)
	ex, err := newExpressions(map[string]string{"#p": "profile"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	pr, err := ex.projection(ptr("age, #p.city, list[2].deep, list[0], missing, list[9]"))
	if err != nil {
		t.Fatal(err)
	}

	want := attributes(t, `{
		"age": {"N": "42"},
		"profile": {"M": {"city": {"S": "London"}}},
		"list": {"L": [{"S": "a"}, {"M": {"deep": {"BOOL": true}}}]}
	}`)
	if got := pr.apply(it); !reflect.DeepEqual(got, want) {
		t.Fatalf("projection:\n%v\nwant\n%v", got, want)
	}

	for expr, want := range map[string]string{
		"a, a.b":    "overlap",
		"a.b, a[0]": "conflict",
		"a,":        "token: <EOF>",
	} {
		ex, err = newExpressions(nil, nil)
		if err != nil {
			t.Fatal(err)
		}

		if _, err = ex.projection(&expr); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", expr, err, want)
		}
	}
}

func TestUpdate(t *testing.T) {
	values := attributes(t, `{
		":one": {"N": "1"}, ":half": {"N": "0.5"}, ":x": {"S": "x"}, ":more": {"L": [{"S": "z"}]},
		":tags": {"SS": ["ops", "dev"]}, ":scores": {"NS": ["2.50"]}, ":zero": {"N": "0"}
	}`)
	names := map[string]string{"#p": "profile"}
	for _, tc := range []struct{ expr, want string }{
		{
			"SET age = age + :one, counter = if_not_exists(counter, :zero) + :one",
			`{"age": {"N": "43"}, "counter": {"N": "1"}}`,
		},
		{"SET age = age - :half", `{"age": {"N": "41.5"}}`},
		{
			"SET list = list_append(list, :more)",
			`{"list": {"L": [{"S": "a"}, {"N": "7"}, {"M": {"deep": {"BOOL": true}}}, {"S": "z"}]}}`,
		},
		{
			"SET #p.city = :x, list[9] = :x",
			`{"profile": {"M": {"city": {"S": "x"}, "zip": {"NULL": true}}},` +
				`"list": {"L": [{"S": "a"}, {"N": "7"}, {"M": {"deep": {"BOOL": true}}}, {"S": "x"}]}}`,
		},
		{
			"REMOVE list[0], list[1], #p.zip, missing",
			`{"list": {"L": [{"M": {"deep": {"BOOL": true}}}]}, "profile": {"M": {"city": {"S": "London"}}}}`,
		},
		{
			"ADD tags :tags, scores :scores, fresh :one",
			`{"tags": {"SS": ["admin", "ops", "dev"]}, "scores": {"NS": ["1", "2.5"]}, "fresh": {"N": "1"}}`,
		},
		{"DELETE tags :tags, scores :scores", `{"tags": {"SS": ["admin"]}, "scores": {"NS": ["1"]}}`},
	} {
		it := attributes(t, sample)
		ex, err := newExpressions(names, values)
		if err != nil {
			t.Fatal(err)
		}

		u, err := ex.update(&tc.expr)
		if err != nil {
			t.Fatalf("%s: %v", tc.expr, err)
		}

		got, err := u.apply(it)
		if err != nil {
			t.Fatalf("%s: %v", tc.expr, err)
		}

		want := attributes(t, sample)
		for k, v := range attributes(t, tc.want) {
			want[k] = v
		}

		if strings.HasPrefix(tc.expr, "REMOVE") {
			delete(want, "missing")
		}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n%v\nwant\n%v", tc.expr, got, want)
		}

		if !reflect.DeepEqual(it, attributes(t, sample)) {
			t.Errorf("%s changed the original item", tc.expr)
		}
	}

	for expr, want := range map[string]string{
		"SET a = :one SET b = :one":         "only be used once",
		"SET a = :one, a = :x":              "overlap",
		"SET a = :one REMOVE a.b":           "overlap",
		"ADD a :x":                          "operator: ADD, operand type: S",
		"DELETE a :one":                     "operator: DELETE, operand type: N",
		"SET a = nope(b)":                   "Invalid function name",
		"UPSERT a = :one":                   `token: "UPSERT"`,
		"SET a = missing":                   "does not exist",
		"SET a = #p + :one":                 "incorrect data type",
		"SET name = list_append(:x, :more)": "incorrect data type",
		"SET age.x = :one":                  "invalid for update",
	} {
		ex, err := newExpressions(names, values)
		if err != nil {
			t.Fatal(err)
		}

		u, err := ex.update(&expr)
		if err == nil {
			_, err = u.apply(attributes(t, sample))
		}

		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", expr, err, want)
		}
	}
}

func TestFormatNumber(t *testing.T) {
	for in, want := range map[string]string{"1.50": "1.5", "-0.125": "-0.125", "100": "100", "1e3": "1000", "0.0": "0"} {
		if got, err := formatNumber(rat(in)); err != nil || got != want {
			t.Errorf("%s: %s, %v", in, got, err)
		}
	}
}
