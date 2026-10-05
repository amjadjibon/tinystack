package tagging

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	if err := Check(map[string]string{"k": "", strings.Repeat("é", 128): strings.Repeat("v", 256)}); err != nil {
		t.Fatal(err)
	}

	for name, tags := range map[string]map[string]string{
		"empty key":  {"": "v"},
		"long key":   {strings.Repeat("k", 129): "v"},
		"long value": {"k": strings.Repeat("v", 257)},
	} {
		if Check(tags) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestMergeAndRemove(t *testing.T) {
	current := map[string]string{"a": "1"}
	merged, err := Merge(current, map[string]string{"a": "2", "b": "3"})
	if err != nil || len(current) != 1 || merged["a"] != "2" || merged["b"] != "3" {
		t.Fatal(current, merged, err)
	}

	many := map[string]string{}
	for i := range Max {
		many[fmt.Sprint(i)] = ""
	}

	if _, err = Merge(many, map[string]string{"extra": ""}); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}

	if left := Remove(merged, []string{"a", "missing"}); len(left) != 1 || left["b"] != "3" {
		t.Fatal(left)
	}
}

func TestCheckSet(t *testing.T) {
	if err := CheckSet(map[string]string{"AWS:name": "v"}); !errors.Is(err, ErrReserved) {
		t.Fatalf("reserved key error = %v", err)
	}

	if err := CheckSet(map[string]string{"awsname": "v"}); err != nil {
		t.Fatal(err)
	}
}

func TestMapAndList(t *testing.T) {
	type tag struct{ k, v *string }

	b, a, one := "b", "a", "1"
	got := Map([]tag{{&b, &one}, {&a, nil}}, func(t tag) (*string, *string) { return t.k, t.v })
	if len(got) != 2 || got["b"] != "1" || got["a"] != "" {
		t.Fatal(got)
	}

	list := List(got, func(k, v string) string { return k + "=" + v })
	if fmt.Sprint(list) != "[a= b=1]" {
		t.Fatal(list)
	}

	if list := List(nil, func(k, v string) string { return k }); list == nil {
		t.Fatal("List returned nil")
	}
}
