package athena

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"

	glueapi "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	"github.com/amjadjibon/tinystack/internal/common"
)

// driverName is the SQLite driver queries run on, which has the Trino
// functions SQLite lacks.
const driverName = "athena-sqlite"

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
	sql.Register(driverName, d)
}

// column is a table column: its name and Hive type.
type column struct{ name, kind string }

func columnsOf(t *glueapi.Table) (data, partitions []column) {
	for _, c := range common.OrElse(t.StorageDescriptor, &glueapi.StorageDescriptor{}).Columns {
		data = append(data, column{strings.ToLower(common.Value(c.Name)), strings.ToLower(common.Value(c.Type))})
	}

	for _, c := range t.PartitionKeys {
		partitions = append(partitions, column{
			strings.ToLower(common.Value(c.Name)),
			strings.ToLower(common.Value(c.Type)),
		})
	}

	return data, partitions
}

// base is a Hive type without its parameters: decimal for decimal(10,2).
func base(kind string) string {
	if i := strings.IndexAny(kind, "(<"); i >= 0 {
		return strings.TrimSpace(kind[:i])
	}

	return kind
}

// athenaType is the type Athena reports for a Hive column type.
func athenaType(kind string) string {
	switch b := base(kind); b {
	case "string", "char", typeVarchar, "":
		return typeVarchar
	case "int":
		return "integer"
	case "struct":
		return "row"
	default:
		return b
	}
}

// value converts a field's text to what SQLite stores for the column's type:
// integers, reals, booleans as 0 or 1, and everything else (dates and
// timestamps included) as text. Text that is not a value of the type is
// NULL, as Hive reads it.
func value(field *string, kind string) any {
	if field == nil {
		return nil
	}

	s := strings.TrimSpace(*field)
	switch base(kind) {
	case "tinyint", "smallint", "int", "integer", "bigint":
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}

		return nil
	case "float", "double", "real", "decimal":
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}

		return nil
	case typeBoolean:
		switch strings.ToLower(s) {
		case "true":
			return int64(1)
		case "false":
			return int64(0)
		}

		return nil
	}

	return *field
}

func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// load creates a table in the attached database it belongs to and fills it
// with its rows, returning the bytes it read.
func (s *Service) load(ctx context.Context, conn *sql.Conn, t *glueapi.Table) (int64, error) {
	f, err := formatOf(t)
	if err != nil {
		return 0, err
	}

	data, partitions := columnsOf(t)
	all := append(append([]column{}, data...), partitions...)
	if len(all) == 0 {
		return 0, fmt.Errorf("the table %s has no columns", common.Value(t.Name))
	}

	names := make([]string, len(data))
	defs := make([]string, len(all))
	marks := make([]string, len(all))
	for i, c := range all {
		decl := athenaType(c.kind)
		if decl == "array" || decl == "map" || decl == "row" {
			decl = "json"
		}

		defs[i], marks[i] = quote(c.name)+" "+decl, "?"
		if i < len(data) {
			names[i] = c.name
		}
	}

	table := quote(common.Value(t.DatabaseName)) + "." + quote(common.Value(t.Name))
	if _, err = conn.ExecContext(ctx, "CREATE TABLE "+table+" ("+strings.Join(defs, ", ")+")"); err != nil {
		return 0, err
	}

	sources, err := s.sources(ctx, t)
	if err != nil {
		return 0, err
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}

	defer func() { _ = tx.Rollback() }()
	// #nosec G202 -- the table's names are quoted identifiers, and its values bound parameters.
	insert, err := tx.PrepareContext(ctx, "INSERT INTO "+table+" VALUES ("+strings.Join(marks, ", ")+")")
	if err != nil {
		return 0, err
	}

	var scanned int64
	for _, src := range sources {
		objects, listErr := s.objectsOf(ctx, t, src)
		if listErr != nil {
			return 0, listErr
		}

		for _, o := range objects {
			body, readErr := s.read(ctx, o)
			if readErr != nil {
				return 0, readErr
			}

			scanned += o.size
			rows, parseErr := f.records(body, names)
			if parseErr != nil {
				return 0, parseErr
			}

			for _, row := range rows {
				args := make([]any, len(all))
				for i, c := range data {
					if i < len(row) {
						args[i] = value(row[i], c.kind)
					}
				}

				for i, c := range partitions {
					args[len(data)+i] = value(&o.values[i], c.kind)
				}

				if _, err = insert.ExecContext(ctx, args...); err != nil {
					return 0, err
				}
			}
		}
	}

	return scanned, tx.Commit()
}

// resultSet is a query's columns, with their Athena types, and rows.
type resultSet struct {
	Columns []resultColumn
	Rows    [][]*string
}

type resultColumn struct{ Name, Type string }

// collect reads a query's rows as Athena renders them.
func collect(rows *sql.Rows) (*resultSet, error) {
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}

	out := &resultSet{Columns: make([]resultColumn, len(types))}
	for i, t := range types {
		out.Columns[i] = resultColumn{Name: t.Name(), Type: strings.ToLower(t.DatabaseTypeName())}
	}

	values := make([]any, len(types))
	targets := make([]any, len(types))
	for i := range values {
		targets[i] = &values[i]
	}

	for rows.Next() {
		if scanErr := rows.Scan(targets...); scanErr != nil {
			return nil, scanErr
		}

		row := make([]*string, len(values))
		for i, v := range values {
			c := &out.Columns[i]
			if c.Type == "" && v != nil {
				c.Type = inferred(v)
			}

			row[i] = render(v, c.Type)
		}

		out.Rows = append(out.Rows, row)
	}

	for i := range out.Columns {
		if out.Columns[i].Type == "" || out.Columns[i].Type == "json" {
			out.Columns[i].Type = typeVarchar
		}
	}

	return out, rows.Err()
}

func inferred(v any) string {
	switch v.(type) {
	case int64:
		return "bigint"
	case float64:
		return "double"
	case bool:
		return typeBoolean
	case time.Time:
		return "timestamp"
	}

	return typeVarchar
}

// render is a value as Athena writes it in results.
func render(v any, kind string) *string {
	switch t := v.(type) {
	case nil:
		return nil
	case int64:
		if kind == typeBoolean {
			return new(strconv.FormatBool(t != 0))
		}

		return new(strconv.FormatInt(t, 10))
	case float64:
		s := strconv.FormatFloat(t, 'f', -1, 64)
		if !strings.ContainsAny(s, ".eEn") && !math.IsInf(t, 0) {
			s += ".0"
		}

		return &s
	case bool:
		return new(strconv.FormatBool(t))
	case []byte:
		return new(string(t))
	case time.Time:
		if kind == "date" {
			return new(t.Format(time.DateOnly))
		}

		return new(t.Format("2006-01-02 15:04:05.000"))
	case string:
		return &t
	}

	return new(fmt.Sprint(v))
}
