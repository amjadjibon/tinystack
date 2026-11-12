package redisparams

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

const (
	memoryDBRedis6  = "memorydb_redis6"
	memoryDBVersion = "6.2"
	yesNo           = "yes,no"
)

// MemoryDB returns the documented engine parameters for a MemoryDB family.
// Source: https://docs.aws.amazon.com/memorydb/latest/devguide/parametergroups.redis.html
func MemoryDB(family string) []Definition {
	defs := slices.Clone(Redis[:3])
	defs[0].DefaultValue = "noeviction"
	defs[1].AllowedValues = "0,20-"
	defs = append(
		defs,
		Definition{
			Name:          "list-compress-depth",
			DataType:      typeInteger,
			AllowedValues: "0-",
			DefaultValue:  "0",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "hll-sparse-max-bytes",
			DataType:      typeInteger,
			AllowedValues: "1-16000",
			DefaultValue:  "3000",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "lfu-log-factor",
			DataType:      typeInteger,
			AllowedValues: "1-",
			DefaultValue:  "10",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "lfu-decay-time",
			DataType:      typeInteger,
			AllowedValues: "0-",
			DefaultValue:  "1",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "active-defrag-max-scan-fields",
			DataType:      typeInteger,
			AllowedValues: "1-1000000",
			DefaultValue:  "1000",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "active-defrag-threshold-upper",
			DataType:      typeInteger,
			AllowedValues: "1-100",
			DefaultValue:  "100",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "client-output-buffer-limit-pubsub-hard-limit",
			DataType:      typeInteger,
			AllowedValues: "0-",
			DefaultValue:  "33554432",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "client-output-buffer-limit-pubsub-soft-limit",
			DataType:      typeInteger,
			AllowedValues: "0-",
			DefaultValue:  "8388608",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "client-output-buffer-limit-pubsub-soft-seconds",
			DataType:      typeInteger,
			AllowedValues: "0-",
			DefaultValue:  "60",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "maxmemory-samples",
			DataType:      typeInteger,
			AllowedValues: "1-",
			DefaultValue:  "3",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "slowlog-max-len",
			DataType:      typeInteger,
			AllowedValues: "0-",
			DefaultValue:  "128",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "activerehashing",
			DataType:      typeString,
			AllowedValues: yesNo,
			DefaultValue:  "yes",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "client-output-buffer-limit-normal-hard-limit",
			DataType:      typeInteger,
			AllowedValues: "0-",
			DefaultValue:  "0",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "client-output-buffer-limit-normal-soft-limit",
			DataType:      typeInteger,
			AllowedValues: "0-",
			DefaultValue:  "0",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "client-output-buffer-limit-normal-soft-seconds",
			DataType:      typeInteger,
			AllowedValues: "0-",
			DefaultValue:  "0",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "tcp-keepalive",
			DataType:      typeInteger,
			AllowedValues: "0-",
			DefaultValue:  "300",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "active-defrag-cycle-min",
			DataType:      typeInteger,
			AllowedValues: "1-75",
			DefaultValue:  "5",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "stream-node-max-bytes",
			DataType:      typeInteger,
			AllowedValues: "0-",
			DefaultValue:  "4096",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "stream-node-max-entries",
			DataType:      typeInteger,
			AllowedValues: "0-",
			DefaultValue:  "100",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "lazyfree-lazy-eviction",
			DataType:      typeString,
			AllowedValues: yesNo,
			DefaultValue:  "no",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "active-defrag-ignore-bytes",
			DataType:      typeInteger,
			AllowedValues: "1048576-",
			DefaultValue:  "104857600",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "lazyfree-lazy-expire",
			DataType:      typeString,
			AllowedValues: yesNo,
			DefaultValue:  "no",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "active-defrag-threshold-lower",
			DataType:      typeInteger,
			AllowedValues: "1-100",
			DefaultValue:  "10",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "active-defrag-cycle-max",
			DataType:      typeInteger,
			AllowedValues: "1-75",
			DefaultValue:  "75",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "lazyfree-lazy-server-del",
			DataType:      typeString,
			AllowedValues: yesNo,
			DefaultValue:  "no",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "slowlog-log-slower-than",
			DataType:      typeInteger,
			AllowedValues: "0-",
			DefaultValue:  "10000",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "set-max-intset-entries",
			DataType:      typeInteger,
			AllowedValues: "0-",
			DefaultValue:  "512",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "tracking-table-max-keys",
			DataType:      typeInteger,
			AllowedValues: "1-100000000",
			DefaultValue:  "1000000",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "acllog-max-len",
			DataType:      typeInteger,
			AllowedValues: "1-10000",
			DefaultValue:  "128",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "active-expire-effort",
			DataType:      typeInteger,
			AllowedValues: "1-10",
			DefaultValue:  "1",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "lazyfree-lazy-user-del",
			DataType:      typeString,
			AllowedValues: yesNo,
			DefaultValue:  "no",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "activedefrag",
			DataType:      typeString,
			AllowedValues: yesNo,
			DefaultValue:  "no",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "client-query-buffer-limit",
			DataType:      typeInteger,
			AllowedValues: "1048576-1073741824",
			DefaultValue:  "1073741824",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
		Definition{
			Name:          "proto-max-bulk-len",
			DataType:      typeInteger,
			AllowedValues: "1048576-536870912",
			DefaultValue:  "536870912",
			IsModifiable:  true,
			Dynamic:       true,
			MinEngineVer:  memoryDBVersion,
		},
	)
	if family != memoryDBRedis6 {
		defs = slices.DeleteFunc(defs, func(d Definition) bool { return d.Name == "activerehashing" })
		defs = append(
			defs,
			Definition{
				Name:          "latency-tracking",
				DataType:      typeString,
				AllowedValues: yesNo,
				DefaultValue:  "no",
				IsModifiable:  true,
				Dynamic:       true,
				MinEngineVer:  "7.0",
			},
		)
	}

	if family != memoryDBRedis6 {
		defs = append(
			defs,
			Definition{
				Name:          "search-enabled",
				DataType:      typeString,
				AllowedValues: yesNo,
				DefaultValue:  "no",
				IsModifiable:  true,
				MinEngineVer:  "7.1",
			},
			Definition{
				Name:          "search-query-timeout-ms",
				DataType:      typeInteger,
				AllowedValues: "1-60000",
				DefaultValue:  "10000",
				IsModifiable:  true,
				Dynamic:       true,
				MinEngineVer:  "7.1",
			},
		)
	}

	encoding := "ziplist"
	if family != memoryDBRedis6 {
		encoding = "listpack"
	}

	for _, entry := range []struct{ name, value string }{
		{"hash-max-" + encoding + "-entries", "512"},
		{"hash-max-" + encoding + "-value", "64"},
		{"zset-max-" + encoding + "-entries", "128"},
		{"zset-max-" + encoding + "-value", "64"},
	} {
		defs = append(
			defs,
			Definition{
				Name:          entry.name,
				DataType:      typeInteger,
				AllowedValues: "0-",
				DefaultValue:  entry.value,
				IsModifiable:  true,
				Dynamic:       true,
				MinEngineVer:  memoryDBVersion,
			},
		)
	}

	slices.SortFunc(defs, func(a, b Definition) int { return strings.Compare(a.Name, b.Name) })
	return defs
}

// Validate checks a parameter against its pinned definition.
func Validate(d *Definition, value string) error {
	valid := false
	switch {
	case d.Name == "notify-keyspace-events":
		valid = strings.Trim(value, "KEg$lshzxeAtmdn") == ""
	case d.DataType == typeInteger:
		n, err := strconv.ParseInt(value, 10, 64)
		if err == nil {
			for _, allowed := range strings.Split(d.AllowedValues, ",") {
				lo, hi, ranged := strings.Cut(allowed, "-")
				lower, _ := strconv.ParseInt(lo, 10, 64)
				upper, _ := strconv.ParseInt(hi, 10, 64)
				valid = valid || ranged && n >= lower && (hi == "" || n <= upper) || !ranged && n == lower
			}
		}

	default:
		valid = slices.Contains(strings.Split(d.AllowedValues, ","), value)
	}

	if !d.IsModifiable || !valid {
		return fmt.Errorf("invalid value %q for parameter %s (allowed: %s)", value, d.Name, d.AllowedValues)
	}

	return nil
}

// Apply sends dynamic engine values in stable order and stops on the first failure.
// Managed buffer limit fields are combined into Redis's native configuration syntax.
func Apply(ctx context.Context, values map[string]string, command func(context.Context, ...any) error) error {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}

	slices.Sort(names)
	for _, name := range names {
		if strings.HasPrefix(name, "client-output-buffer-limit-") {
			continue
		}

		if err := command(ctx, "CONFIG", "SET", name, values[name]); err != nil {
			return err
		}
	}

	for _, class := range []string{"normal", "pubsub"} {
		prefix := "client-output-buffer-limit-" + class + "-"
		hard, ok := values[prefix+"hard-limit"]
		if !ok {
			continue
		}

		value := class + " " + hard + " " + values[prefix+"soft-limit"] + " " + values[prefix+"soft-seconds"]
		if err := command(ctx, "CONFIG", "SET", "client-output-buffer-limit", value); err != nil {
			return err
		}
	}

	return nil
}
