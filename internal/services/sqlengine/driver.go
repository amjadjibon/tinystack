// Package sqlengine registers the shared SQLite driver and Trino scalar functions.
package sqlengine

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"regexp"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// DriverName is the shared SQL driver.
const DriverName = "athena-sqlite"

func init() {
	d := &sqlite.Driver{}
	text := func(v driver.Value) string {
		switch t := v.(type) {
		case string:
			return t
		case []byte:
			return string(t)
		case nil:
			return ""
		}

		return fmt.Sprint(v)
	}

	pattern := func(v driver.Value) (*regexp.Regexp, error) { return regexp.Compile(text(v)) }
	d.MustRegisterDeterministicScalarFunction("regexp_like", 2, func(_ *sqlite.FunctionContext, args []driver.Value) (
		driver.Value, error,
	) {
		re, err := pattern(args[1])
		if err != nil || args[0] == nil {
			return nil, err
		}

		return re.MatchString(text(args[0])), nil
	})
	d.MustRegisterDeterministicScalarFunction("regexp_extract", -1, func(_ *sqlite.FunctionContext,
		args []driver.Value,
	) (driver.Value, error) {
		if len(args) < 2 || len(args) > 3 {
			return nil, fmt.Errorf("regexp_extract takes 2 or 3 arguments")
		}

		re, err := pattern(args[1])
		if err != nil || args[0] == nil {
			return nil, err
		}

		group := 0
		if len(args) == 3 {
			n, _ := args[2].(int64)
			group = int(n)
		}

		m := re.FindStringSubmatch(text(args[0]))
		if m == nil || group >= len(m) {
			return nil, nil //nolint:nilnil // no match is SQL NULL.
		}

		return m[group], nil
	})
	d.MustRegisterDeterministicScalarFunction("strpos", 2, func(_ *sqlite.FunctionContext, args []driver.Value) (
		driver.Value, error,
	) {
		return int64(strings.Index(text(args[0]), text(args[1])) + 1), nil
	})
	d.MustRegisterDeterministicScalarFunction("starts_with", 2, func(_ *sqlite.FunctionContext,
		args []driver.Value,
	) (driver.Value, error) {
		return strings.HasPrefix(text(args[0]), text(args[1])), nil
	})
	d.MustRegisterDeterministicScalarFunction("split_part", 3, func(_ *sqlite.FunctionContext,
		args []driver.Value,
	) (driver.Value, error) {
		n, _ := args[2].(int64)
		parts := strings.Split(text(args[0]), text(args[1]))
		if n < 1 || int(n) > len(parts) {
			return nil, nil //nolint:nilnil // a part past the end is SQL NULL.
		}

		return parts[n-1], nil
	})

	d.MustRegisterDeterministicScalarFunction(
		"bin",
		2,
		func(_ *sqlite.FunctionContext, a []driver.Value) (driver.Value, error) {
			n, ok := a[0].(int64)
			width, valid := a[1].(int64)
			if !ok || !valid || width <= 0 {
				return nil, fmt.Errorf("bin requires a timestamp and a positive interval")
			}

			remainder := n % width
			if remainder < 0 {
				remainder += width
			}

			return n - remainder, nil
		},
	)
	d.MustRegisterDeterministicScalarFunction(
		"from_milliseconds",
		1,
		func(_ *sqlite.FunctionContext, a []driver.Value) (driver.Value, error) {
			n, ok := a[0].(int64)
			if !ok {
				return nil, fmt.Errorf("from_milliseconds requires bigint")
			}

			return n * int64(time.Millisecond), nil
		},
	)
	d.MustRegisterDeterministicScalarFunction(
		"to_milliseconds",
		1,
		func(_ *sqlite.FunctionContext, a []driver.Value) (driver.Value, error) {
			n, ok := a[0].(int64)
			if !ok {
				return nil, fmt.Errorf("to_milliseconds requires timestamp")
			}

			return n / int64(time.Millisecond), nil
		},
	)
	d.MustRegisterDeterministicScalarFunction(
		"date_trunc",
		2,
		func(_ *sqlite.FunctionContext, a []driver.Value) (driver.Value, error) {
			n, ok := a[1].(int64)
			if !ok {
				return nil, fmt.Errorf("date_trunc requires timestamp")
			}

			t := time.Unix(0, n).UTC()
			switch strings.ToLower(text(a[0])) {
			case "second":
				t = t.Truncate(time.Second)
			case "minute":
				t = t.Truncate(time.Minute)
			case "hour":
				t = t.Truncate(time.Hour)
			case "day":
				t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
			case "month":
				t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
			case "year":
				t = time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
			default:
				return nil, fmt.Errorf("unsupported date_trunc unit: %s", text(a[0]))
			}

			return t.UnixNano(), nil
		},
	)

	sql.Register(DriverName, d)
}
