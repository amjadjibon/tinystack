package redisparams

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestMemoryDBValidation(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		valid       bool
	}{
		{"timeout", "19", false},
		{"timeout", "20", true},
		{"timeout", "0", true},
		{"timeout", "-1", false},
		{"maxmemory-policy", "noeviction", true},
		{"maxmemory-policy", "unknown", false},
		{"notify-keyspace-events", "AKE", true},
		{"notify-keyspace-events", "?", false},
		{"active-expire-effort", "11", false},
		{"active-expire-effort", "10", true},
	} {
		defs := MemoryDB("memorydb_redis6")
		i := slices.IndexFunc(defs, func(d Definition) bool { return d.Name == tc.name })
		if err := Validate(&defs[i], tc.value); (err == nil) != tc.valid {
			t.Errorf("%+v: %v", tc, err)
		}
	}

	for _, family := range []string{"memorydb_redis6", "memorydb_redis7", "memorydb_valkey7"} {
		for _, d := range MemoryDB(family) {
			if err := Validate(&d, d.DefaultValue); err != nil {
				t.Errorf("%s: %v", family, err)
			}
		}
	}
}

func TestApply(t *testing.T) {
	var commands []string
	err := Apply(context.Background(), map[string]string{
		"timeout": "30", "client-output-buffer-limit-pubsub-hard-limit": "100",
		"client-output-buffer-limit-pubsub-soft-limit": "50", "client-output-buffer-limit-pubsub-soft-seconds": "60",
	}, func(_ context.Context, args ...any) error { commands = append(commands, fmt.Sprint(args)); return nil })
	if err != nil ||
		fmt.Sprint(commands) != "[[CONFIG SET timeout 30] [CONFIG SET client-output-buffer-limit pubsub 100 50 60]]" {
		t.Fatal(commands, err)
	}

	expected := errors.New("engine rejected value")
	if err = Apply(context.Background(), map[string]string{"timeout": "30"}, func(context.Context, ...any) error {
		return expected
	}); !errors.Is(err, expected) {
		t.Fatal(err)
	}
}
