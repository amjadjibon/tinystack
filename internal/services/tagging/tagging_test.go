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
