package timestreamquery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamquery"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/sqlengine"
	"github.com/amjadjibon/tinystack/internal/services/timestreamwrite"
)

const (
	kindDouble    = "DOUBLE"
	kindBigint    = "BIGINT"
	kindTimestamp = "TIMESTAMP"
	kindBoolean   = "BOOLEAN"
	kindVarchar   = "VARCHAR"
	functionAgo   = "ago"
	functionNow   = "now"
	kindMulti     = "MULTI"
)

const timestampLayout = "2006-01-02 15:04:05.000000000"

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func duration(s string) (int64, error) {
	d, err := time.ParseDuration(s)
	if strings.HasSuffix(s, "d") {
		n, e := strconv.ParseInt(strings.TrimSuffix(s, "d"), 10, 64)
		return n * int64(24*time.Hour), e
	}

	return int64(d), err
}

// translate accepts a read-only scalar SQL subset. Token-based rewriting keeps strings and quoted names intact.
func translate(query string, now time.Time) (statement, database, tableName string, translateErr error) {
	tokens, err := lex(query)
	if err != nil {
		return "", "", "", invalid("SQL: %s", err)
	}

	var ts []token
	for _, t := range tokens {
		if t.kind != tokenSpace {
			ts = append(ts, t)
		}
	}

	if len(ts) == 0 || !ts[0].is("select") {
		return "", "", "", invalid("Unsupported SQL construct: only SELECT is supported")
	}

	functions := map[string]bool{
		"avg":               true,
		"sum":               true,
		"count":             true,
		"min":               true,
		"max":               true,
		"abs":               true,
		"round":             true,
		"coalesce":          true,
		"nullif":            true,
		"lower":             true,
		"upper":             true,
		functionAgo:         true,
		functionNow:         true,
		"bin":               true,
		"from_milliseconds": true,
		"to_milliseconds":   true,
		"date_trunc":        true,
	}

	var out []string
	var db, table string
	inFrom := false
	depth := 0
	for i := 0; i < len(ts); i++ {
		t := ts[i]
		if t.text == "(" {
			depth++
		}

		if t.text == ")" {
			depth--
		}

		if t.is("where") || t.is("group") || t.is("order") || t.is("having") || t.is("limit") {
			inFrom = false
		}

		if inFrom && depth == 0 && t.text == "," {
			return "", "", "", invalid("Unsupported SQL construct: comma join")
		}

		wildcard := i == 1
		countStar := i > 1 && ts[i-1].text == "(" && ts[i-2].is("count")
		if t.text == "*" && !wildcard && !countStar {
			return "", "", "", invalid("Unsupported SQL operator: multiplication")
		}

		if t.kind == tokenParam || t.text == ";" || strings.HasPrefix(t.text, "`") || t.text == "/" || t.text == "%" ||
			t.text == "+" ||
			t.text == "-" {
			return "", "", "", invalid("Unsupported SQL construct: %s", t.text)
		}

		if t.kind == tokenWord {
			word := strings.ToLower(t.value)
			switch word {
			case "join",
				"union",
				"intersect",
				"except",
				"over",
				"with",
				"pragma",
				"attach",
				"cast",
				"like",
				"glob",
				"offset",
				"collate":
				return "", "", "", invalid("Unsupported SQL construct: %s", t.text)
			}

			if t.is("select") && i != 0 {
				return "", "", "", invalid("Unsupported SQL construct: subquery")
			}

			if i+1 < len(ts) && ts[i+1].text == "(" {
				if !functions[word] && word != "in" {
					return "", "", "", invalid("Unsupported SQL function: %s", t.text)
				}

				if word == functionNow && i+2 < len(ts) && ts[i+2].text == ")" {
					out = append(out, strconv.FormatInt(now.UnixNano(), 10))
					i += 2
					continue
				}

				if word == functionAgo {
					if i+4 >= len(ts) || ts[i+2].kind != tokenNumber || ts[i+4].text != ")" {
						return "", "", "", invalid("Unsupported SQL construct: ago duration")
					}

					d, e := duration(ts[i+2].text + ts[i+3].text)
					if e != nil {
						return "", "", "", invalid("Unsupported SQL duration: %s", ts[i+3].text)
					}

					out = append(out, strconv.FormatInt(now.UnixNano()-d, 10))
					i += 4
					continue
				}
			}
		}

		if t.is("from") {
			if db != "" || i+3 >= len(ts) || ts[i+2].text != "." {
				return "", "", "", invalid("Unsupported SQL FROM: use database.table")
			}

			var ok bool
			ok = ts[i+1].identifier()
			db = ts[i+1].value
			if !ok {
				return "", "", "", invalid("Unsupported SQL database")
			}

			ok = ts[i+3].identifier()
			table = ts[i+3].value
			if !ok {
				return "", "", "", invalid("Unsupported SQL table")
			}

			inFrom = true
			out = append(out, "FROM", quote("points"))
			i += 3
			continue
		}

		if t.is("measure_value") && i+3 < len(ts) && ts[i+1].text == ":" && ts[i+2].text == ":" {
			kind := strings.ToLower(ts[i+3].value)
			switch kind {
			case "double", "bigint", "varchar", "boolean", "timestamp":
				out = append(out, quote("measure_value::"+kind))
				i += 3
				continue
			default:
				return "", "", "", invalid("Unsupported SQL measure cast: %s", kind)
			}
		}

		if t.kind == tokenNumber && i+1 < len(ts) && ts[i+1].kind == tokenWord {
			if d, e := duration(t.text + ts[i+1].text); e == nil {
				out = append(out, strconv.FormatInt(d, 10))
				i++
				continue
			}
		}

		if t.text == ":" || t.text == "|" || t.text == "&" || t.text == "~" {
			return "", "", "", invalid("Unsupported SQL operator: %s", t.text)
		}

		out = append(out, t.text)
	}

	return strings.Join(out, " "), db, table, nil
}

type queryColumn struct{ name, kind string }

func scalar(value, kind string) any {
	switch kind {
	case kindDouble:
		v, _ := strconv.ParseFloat(value, 64)
		return v
	case kindTimestamp:
		v, _ := strconv.ParseInt(value, 10, 64)
		return v * int64(time.Millisecond)
	case kindBigint:
		v, _ := strconv.ParseInt(value, 10, 64)
		return v
	case kindBoolean:
		v, _ := strconv.ParseBool(value)
		return v
	default:
		return value
	}
}

func pointValues(r *timestreamwrite.StoredRecord) map[string]any {
	out := map[string]any{"time": r.Time.UnixNano(), "measure_name": r.MeasureName}
	for _, d := range r.Dimensions {
		out[common.Value(d.Name)] = common.Value(d.Value)
	}

	if r.MeasureType == kindMulti {
		for _, m := range r.Measures {
			out[common.Value(m.Name)] = scalar(common.Value(m.Value), common.Value(m.Type))
		}
	} else {
		out["measure_value::"+strings.ToLower(r.MeasureType)] = scalar(r.MeasureValue, r.MeasureType)
	}

	return out
}

func (s *Service) execute(ctx context.Context, query string, now time.Time) (*api.QueryResponse, error) {
	statement, database, table, err := translate(query, now)
	if err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var points []*timestreamwrite.StoredRecord
	if database != "" {
		points, err = s.timestream.ReadRecords(ctx, scope, database, table)
		if err != nil {
			var e *awserror.Error
			if errors.As(err, &e) && e.Code == "ResourceNotFoundException" {
				return nil, invalid("SQL table: %s", e.Message)
			}

			return nil, err
		}
	}

	db, err := sql.Open(sqlengine.DriverName, ":memory:")
	if err != nil {
		return nil, err
	}

	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	columns := []queryColumn{{"time", kindTimestamp}, {"measure_name", kindVarchar}}
	seen := map[string]string{}
	for _, c := range columns {
		seen[c.name] = c.kind
	}

	for _, r := range points {
		if r.MeasureType != kindMulti {
			name := "measure_value::" + strings.ToLower(r.MeasureType)
			if seen[name] == "" {
				columns = append(columns, queryColumn{name, r.MeasureType})
				seen[name] = r.MeasureType
			}
		}

		for _, d := range r.Dimensions {
			name := common.Value(d.Name)
			if seen[name] == "" {
				columns = append(columns, queryColumn{name, kindVarchar})
				seen[name] = kindVarchar
			}
		}

		for _, m := range r.Measures {
			name, kind := common.Value(m.Name), common.Value(m.Type)
			if old := seen[name]; old != "" && old != kind {
				return nil, invalid("Conflicting column type: %s", name)
			}

			if seen[name] == "" {
				columns = append(columns, queryColumn{name, kind})
				seen[name] = kind
			}
		}
	}

	defs, marks := []string{}, []string{}
	for _, c := range columns {
		defs = append(defs, quote(c.name)+" "+c.kind)
		marks = append(marks, "?")
	}

	if _, err = db.ExecContext(ctx, "CREATE TABLE points ("+strings.Join(defs, ",")+")"); err != nil {
		return nil, err
	}

	var scanned int64
	for _, r := range points {
		values := pointValues(r)
		args := make([]any, len(columns))
		for i, c := range columns {
			args[i] = values[c.name]
			scanned += int64(len(fmt.Sprint(args[i])))
		}

		// #nosec G202 -- identifiers are quoted and all values use bound parameters.
		if _, err = db.ExecContext(
			ctx,
			"INSERT INTO points VALUES ("+strings.Join(marks, ",")+")",
			args...); err != nil {
			return nil, err
		}
	}

	// SQLite treats unknown double-quoted names as string literals; Timestream rejects them.
	sqlTokens, _ := lex(statement)
	aliases := map[string]bool{"points": true}
	for i, t := range sqlTokens {
		if t.is("as") {
			for _, next := range sqlTokens[i+1:] {
				if next.kind == tokenSpace {
					continue
				}

				aliases[next.value] = true
				break
			}
		}
	}

	for _, t := range sqlTokens {
		if t.kind == tokenQuoted && seen[t.value] == "" && !aliases[t.value] {
			return nil, invalid("Unknown SQL column: %s", t.value)
		}
	}

	rows, err := db.QueryContext(ctx, statement)
	if err != nil {
		return nil, invalid("SQL: %s", err)
	}

	defer func() { _ = rows.Close() }()
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}

	out := &api.QueryResponse{
		QueryId:    new(common.RandomID(32)),
		Rows:       []api.Row{},
		ColumnInfo: []api.ColumnInfo{},
		QueryStatus: &api.QueryStatus{
			ProgressPercentage:     new(float64(100)),
			CumulativeBytesScanned: new(scanned),
			CumulativeBytesMetered: new(scanned),
		},
	}

	projected := projectionTypes(query, seen)
	for i, c := range types {
		kind := strings.ToUpper(c.DatabaseTypeName())
		if kind == "" && i < len(projected) {
			kind = projected[i]
		}

		out.ColumnInfo = append(
			out.ColumnInfo,
			api.ColumnInfo{Name: new(c.Name()), Type: &api.Type{ScalarType: new(kind)}},
		)
	}

	for rows.Next() {
		values := make([]any, len(types))
		targets := make([]any, len(types))
		for i := range values {
			targets[i] = &values[i]
		}

		if scanErr := rows.Scan(targets...); scanErr != nil {
			return nil, scanErr
		}

		row := api.Row{Data: []api.Datum{}}
		for i, v := range values {
			kind := common.Value(out.ColumnInfo[i].Type.ScalarType)
			if kind == "" && v != nil {
				kind = kindVarchar
				switch v.(type) {
				case int64:
					kind = kindBigint
				case float64:
					kind = kindDouble
				}

				out.ColumnInfo[i].Type.ScalarType = new(kind)
			}

			datum := api.Datum{}
			if v == nil {
				datum.NullValue = new(true)
			} else {
				value := fmt.Sprint(v)
				if n, ok := v.(int64); ok {
					if kind == kindTimestamp {
						value = time.Unix(0, n).UTC().Format(timestampLayout)
					}

					if kind == kindBoolean {
						value = strconv.FormatBool(n != 0)
					}
				}

				datum.ScalarValue = new(value)
			}

			row.Data = append(row.Data, datum)
		}

		out.Rows = append(out.Rows, row)
	}

	for i := range out.ColumnInfo {
		if common.Value(out.ColumnInfo[i].Type.ScalarType) == "" {
			out.ColumnInfo[i].Type.ScalarType = new(kindVarchar)
		}
	}

	if rowErr := rows.Err(); rowErr != nil {
		return nil, invalid("SQL: %s", rowErr)
	}

	return out, nil
}

// projectionTypes retains scalar types across SQLite expressions (whose declared types SQLite omits).
func projectionTypes(query string, columns map[string]string) []string {
	tokens, _ := lex(query)
	var groups [][]token
	var current []token
	depth := 0
	for _, t := range tokens {
		if t.kind == tokenSpace || t.is("select") {
			continue
		}

		if depth == 0 && t.is("from") {
			break
		}

		if t.text == "(" {
			depth++
		}

		if t.text == ")" {
			depth--
		}

		if depth == 0 && t.text == "," {
			groups = append(groups, current)
			current = nil
			continue
		}

		current = append(current, t)
	}

	groups = append(groups, current)
	var kinds []string
	for _, g := range groups {
		kind := ""
		for i, t := range g {
			if t.is("as") {
				break
			}

			if t.is("measure_value") && i+3 < len(g) && g[i+1].text == ":" && g[i+2].text == ":" {
				kind = strings.ToUpper(g[i+3].value)
			}

			if t.is("true") || t.is("false") {
				kind = kindBoolean
			}

			if k := columns[t.value]; k != "" {
				kind = k
			}

			if t.kind == tokenWord {
				switch strings.ToLower(t.value) {
				case "bin", functionNow, functionAgo, "date_trunc", "from_milliseconds":
					kind = kindTimestamp
				case "count", "to_milliseconds":
					kind = kindBigint
				case "avg":
					kind = kindDouble
				}

				if kind != "" && (t.is("bin") || t.is(functionNow) || t.is(functionAgo) || t.is("date_trunc") ||
					t.is("from_milliseconds") || t.is("count") || t.is("to_milliseconds") || t.is("avg")) {
					break
				}
			}
		}

		kinds = append(kinds, kind)
	}

	return kinds
}
