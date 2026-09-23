package dynamodb

import (
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
)

func TestAttributeValidation(t *testing.T) {
	for _, tc := range []struct {
		value api.AttributeValue
		name  string
		valid bool
	}{
		{
			name: "nested map",
			value: api.AttributeValue{M: ptr(map[string]api.AttributeValue{
				"nested": {L: ptr([]api.AttributeValue{{BOOL: ptr(false)}})},
			})}, valid: true,
		},
		{name: "binary set", value: api.AttributeValue{BS: ptr([][]byte{{0, 1}, {2, 3}})}, valid: true},
		{name: "number set", value: api.AttributeValue{NS: ptr([]string{"1", "2"})}, valid: true},
		{name: "equivalent numbers", value: api.AttributeValue{NS: ptr([]string{"1", "1.0"})}, valid: false},
		{name: "invalid number set", value: api.AttributeValue{NS: ptr([]string{"nan"})}, valid: false},
		{name: "duplicate strings", value: api.AttributeValue{SS: ptr([]string{"one", "one"})}, valid: false},
		{name: "bad child", value: api.AttributeValue{L: ptr([]api.AttributeValue{{}})}, valid: false},
		{name: "bad map child", value: api.AttributeValue{M: ptr(map[string]api.AttributeValue{"bad": {}})}, valid: false},
		{
			name:  "empty map name",
			value: api.AttributeValue{M: ptr(map[string]api.AttributeValue{"": {S: ptr("x")}})}, valid: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := attributeSize(tc.value, 0)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
	for _, v := range []string{"1e126", "1e-131", "123456789012345678901234567890123456789", "1e-201"} {
		t.Run(v, func(t *testing.T) {
			if _, err := number(v); err == nil {
				t.Fatal("out-of-range number accepted")
			}
		})
	}
	a := api.AttributeValue{S: ptr("leaf")}
	for range 34 {
		a = api.AttributeValue{L: ptr([]api.AttributeValue{a})}
	}
	if _, err := attributeSize(a, 0); err == nil {
		t.Fatal("excessive depth accepted")
	}
}
