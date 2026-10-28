package tabular

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress"
	"github.com/parquet-go/parquet-go/compress/gzip"
	"github.com/parquet-go/parquet-go/compress/snappy"
	"github.com/parquet-go/parquet-go/compress/uncompressed"
	"github.com/parquet-go/parquet-go/compress/zstd"
	"github.com/parquet-go/parquet-go/format"
)

const (
	hiveTinyint   = "tinyint"
	hiveSmallint  = "smallint"
	hiveInt       = "int"
	hiveBigint    = "bigint"
	hiveString    = "string"
	hiveDate      = "date"
	hiveTimestamp = "timestamp"
	hiveBoolean   = "boolean"
	hiveFloat     = "float"
	hiveDouble    = "double"
	hiveBinary    = "binary"
)

// Column is a named Hive column. Rows use nil for SQL NULL and JSON text for nested values.
type Column struct{ Name, Type string }

// Infer returns the columns encoded in a Parquet object's footer, without reading its pages.
func Infer(body []byte) ([]Column, error) {
	f, err := parquet.OpenFile(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, err
	}

	var out []Column
	for _, field := range f.Schema().Fields() {
		out = append(out, Column{field.Name(), hiveType(field)})
	}

	return out, nil
}

func logicalType[T format.LogicalTypeValue](n parquet.Node) T {
	var value T
	if logical := n.Type().LogicalType(); logical != nil {
		value, _ = logical.Value.(T)
	}

	return value
}

func hiveType(n parquet.Node) string {
	if !n.Leaf() {
		if logicalType[*format.ListType](n) != nil {
			return "array<" + hiveType(n.Fields()[0].Fields()[0]) + ">"
		}

		if logicalType[*format.MapType](n) != nil {
			f := n.Fields()[0].Fields()
			return "map<" + hiveType(f[0]) + "," + hiveType(f[1]) + ">"
		}

		var fields []string
		for _, f := range n.Fields() {
			fields = append(fields, f.Name()+":"+hiveType(f))
		}

		return "struct<" + strings.Join(fields, ",") + ">"
	}

	if l := n.Type().LogicalType(); l != nil {
		switch value := l.Value.(type) {
		case *format.IntType:
			switch value.BitWidth {
			case 8:
				return hiveTinyint
			case 16:
				return hiveSmallint
			case 32:
				return hiveInt
			default:
				return hiveBigint
			}
		case *format.StringType:
			return hiveString
		case *format.DateType:
			return hiveDate
		case *format.TimestampType:
			return hiveTimestamp
		case *format.DecimalType:
			return fmt.Sprintf("decimal(%d,%d)", value.Precision, value.Scale)
		}
	}

	switch n.Type().Kind() {
	case parquet.Boolean:
		return hiveBoolean
	case parquet.Int32:
		return hiveInt
	case parquet.Int64:
		return hiveBigint
	case parquet.Float:
		return hiveFloat
	case parquet.Double:
		return hiveDouble
	}

	return hiveBinary
}

func readParquet(body []byte, columns []string) ([][]*string, error) {
	var out [][]*string
	err := StreamParquet(
		bytes.NewReader(body),
		int64(len(body)),
		columns,
		func(row []*string) error { out = append(out, row); return nil },
	)
	return out, err
}

// StreamParquet reads only the requested columns and calls visit once per row.
// It supports range-backed readers without loading an entire object into memory.
func StreamParquet(reader io.ReaderAt, size int64, columns []string, visit func([]*string) error) error {
	f, err := parquet.OpenFile(reader, size)
	if err != nil {
		return fmt.Errorf("HIVE_CURSOR_ERROR: %w", err)
	}

	projected := parquet.Group{}
	for _, field := range f.Schema().Fields() {
		if slices.ContainsFunc(columns, func(c string) bool { return strings.EqualFold(c, field.Name()) }) {
			projected[field.Name()] = field
		}
	}

	readerSchema := f.Schema()
	if len(projected) > 0 {
		readerSchema = parquet.NewSchema("projection", projected)
	}

	r := parquet.NewReader(f, readerSchema)
	defer func() { _ = r.Close() }()
	types := map[string]parquet.Node{}
	for _, f := range f.Schema().Fields() {
		types[strings.ToLower(f.Name())] = f
	}

	for {
		raw := make([]parquet.Row, 1)
		count, readErr := r.ReadRows(raw)
		if count == 0 && errors.Is(readErr, io.EOF) {
			break
		}

		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}

		cols := make([][]parquet.Value, len(readerSchema.Columns()))
		raw[0].Range(func(i int, values []parquet.Value) bool { cols[i] = values; return true })
		decoded := decodeNode(readerSchema, cols, 0, 0)
		obj, _ := decoded.(map[string]any)
		lower := map[string]any{}
		for k, v := range obj {
			lower[strings.ToLower(k)] = v
		}

		row := make([]*string, len(columns))
		for i, c := range columns {
			v := lower[strings.ToLower(c)]
			if v == nil {
				continue
			}

			node := types[strings.ToLower(c)]
			text, err := parquetText(v, node)
			if err != nil {
				return err
			}

			row[i] = new(text)
		}

		if err := visit(row); err != nil {
			return err
		}
	}

	return nil
}

func parquetText(v any, n parquet.Node) (string, error) {
	if n == nil {
		return fmt.Sprint(v), nil
	}

	kind := hiveType(n)
	if !n.Leaf() {
		b, err := json.Marshal(normalizeNested(v, n))
		return string(b), err
	}

	switch {
	case kind == hiveDate:
		return time.Unix(int64Number(v)*86400, 0).UTC().Format(time.DateOnly), nil
	case kind == hiveTimestamp:
		l := logicalType[*format.TimestampType](n)
		factor := int64(1000000)
		if _, ok := l.Unit.Value.(*format.MicroSeconds); ok {
			factor = 1000
		}

		if _, ok := l.Unit.Value.(*format.NanoSeconds); ok {
			factor = 1
		}

		return time.Unix(0, int64Number(v)*factor).UTC().Format("2006-01-02 15:04:05.999999999"), nil
	case strings.HasPrefix(kind, "decimal"):
		scale := logicalType[*format.DecimalType](n).Scale
		return decimalText(v, int(scale)), nil
	}

	switch x := v.(type) {
	case []byte:
		return string(x), nil
	default:
		return fmt.Sprint(v), nil
	}
}

func int64Number(v any) int64 {
	switch n := v.(type) {
	case int32:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}

	return 0
}

func codec(name string) (compress.Codec, error) {
	switch strings.ToUpper(name) {
	case "", "GZIP":
		return &gzip.Codec{}, nil
	case "SNAPPY":
		return &snappy.Codec{}, nil
	case "ZSTD":
		return &zstd.Codec{}, nil
	case "NONE", "UNCOMPRESSED":
		return &uncompressed.Codec{}, nil
	}

	return nil, fmt.Errorf("NOT_SUPPORTED: compression %s", name)
}

func nodeFor(kind string) (parquet.Node, error) {
	k := strings.ToLower(kind)
	if (strings.HasPrefix(k, "array<") || strings.HasPrefix(k, "map<") || strings.HasPrefix(k, "struct<")) &&
		!strings.HasSuffix(k, ">") {
		return nil, fmt.Errorf("invalid Hive type %s", kind)
	}

	switch {
	case k == hiveBoolean:
		return parquet.Leaf(parquet.BooleanType), nil
	case k == hiveTinyint:
		return parquet.Int(8), nil
	case k == hiveSmallint:
		return parquet.Int(16), nil
	case k == hiveInt || k == "integer":
		return parquet.Int(32), nil
	case k == hiveBigint:
		return parquet.Int(64), nil
	case k == hiveFloat || k == "real":
		return parquet.Leaf(parquet.FloatType), nil
	case k == hiveDouble:
		return parquet.Leaf(parquet.DoubleType), nil
	case k == hiveDate:
		return parquet.Date(), nil
	case k == hiveTimestamp:
		return parquet.Timestamp(parquet.Microsecond), nil
	case k == hiveBinary || k == "varbinary":
		return parquet.Leaf(parquet.ByteArrayType), nil
	case strings.HasPrefix(k, "decimal("):
		var p, s int
		if _, err := fmt.Sscanf(k, "decimal(%d,%d)", &p, &s); err != nil {
			return nil, err
		}

		if p < 1 || p > 38 || s < 0 || s > p {
			return nil, fmt.Errorf("invalid decimal precision or scale")
		}

		if p > 18 {
			return parquet.Decimal(s, p, parquet.FixedLenByteArrayType(16)), nil
		}

		return parquet.Decimal(s, p, parquet.Int64Type), nil
	case strings.HasPrefix(k, "array<"):
		n, err := nodeFor(k[6 : len(k)-1])
		if err != nil {
			return nil, err
		}

		return parquet.List(parquet.Optional(n)), nil
	case strings.HasPrefix(k, "map<"):
		parts := splitTypes(k[4 : len(k)-1])
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid map type %s", k)
		}

		key, err := nodeFor(parts[0])
		if err != nil {
			return nil, err
		}

		val, err := nodeFor(parts[1])
		if err != nil {
			return nil, err
		}

		return parquet.Map(key, parquet.Optional(val)), nil
	case strings.HasPrefix(k, "struct<"):
		group := parquet.Group{}
		for _, part := range splitTypes(k[7 : len(k)-1]) {
			name, typ, ok := strings.Cut(part, ":")
			if !ok {
				return nil, fmt.Errorf("invalid struct type")
			}

			n, err := nodeFor(typ)
			if err != nil {
				return nil, err
			}

			group[name] = parquet.Optional(n)
		}

		return group, nil
	case k == "string", k == "varchar", k == "char",
		strings.HasPrefix(k, "varchar("), strings.HasPrefix(k, "char("):
		return parquet.String(), nil
	}

	return nil, fmt.Errorf("NOT_SUPPORTED: Hive type %s", kind)
}

func splitTypes(s string) []string {
	var out []string
	start, depth := 0, 0
	for i, c := range s {
		switch c {
		case '<', '(':
			depth++
		case '>', ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}

	return append(out, s[start:])
}

func parquetValue(s string, n parquet.Node) (any, error) {
	if !n.Leaf() {
		var v any
		d := json.NewDecoder(strings.NewReader(s))
		d.UseNumber()
		if err := d.Decode(&v); err != nil {
			return nil, err
		}

		return nestedValue(v, n)
	}

	k := hiveType(n)
	switch {
	case k == hiveBoolean:
		return strconv.ParseBool(s)
	case k == hiveTinyint || k == hiveSmallint || k == hiveInt:
		bits := 32
		if logical := logicalType[*format.IntType](n); logical != nil {
			bits = int(logical.BitWidth)
		}

		v, err := strconv.ParseInt(s, 10, bits)
		// #nosec G115 -- ParseInt enforces a signed width no greater than 32.
		return int32(v), err
	case k == hiveBigint:
		return strconv.ParseInt(s, 10, 64)
	case k == hiveFloat:
		v, err := strconv.ParseFloat(s, 32)
		return float32(v), err
	case k == hiveDouble:
		return strconv.ParseFloat(s, 64)
	case k == hiveDate:
		v, err := time.Parse(time.DateOnly, s)
		// #nosec G115 -- Hive dates parsed by time.Parse fit signed 32-bit days.
		return int32(v.Unix() / 86400), err
	case k == hiveTimestamp:
		v, err := time.Parse("2006-01-02 15:04:05.999999999", s)
		return v.UnixMicro(), err
	case strings.HasPrefix(k, "decimal"):
		dec := logicalType[*format.DecimalType](n)
		value, err := parseDecimal(s, int(dec.Precision), int(dec.Scale))
		if err != nil {
			return nil, err
		}

		if n.Type().Kind() == parquet.Int64 {
			return value.Int64(), nil
		}

		out := make([]byte, n.Type().Length())
		encoded := new(big.Int).Set(value)
		if encoded.Sign() < 0 {
			encoded.Add(encoded, new(big.Int).Lsh(big.NewInt(1), uint(8*len(out))))
		}

		encoded.FillBytes(out)
		return out, nil
	case k == hiveBinary:
		return []byte(s), nil
	}

	return s, nil
}

func nestedValue(v any, n parquet.Node) (any, error) {
	if v == nil {
		return nil, nil //nolint:nilnil // A nested SQL NULL has no scalar value.
	}

	if n.Leaf() {
		return parquetValue(fmt.Sprint(v), n)
	}

	if logicalType[*format.ListType](n) != nil {
		a, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("expected array")
		}

		for i, x := range a {
			y, err := nestedValue(x, n.Fields()[0].Fields()[0])
			if err != nil {
				return nil, err
			}

			a[i] = y
		}

		return a, nil
	}

	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected object")
	}

	if logicalType[*format.MapType](n) != nil {
		fields := n.Fields()[0].Fields()
		out := map[any]any{}
		for k, x := range m {
			key, err := nestedValue(k, fields[0])
			if err != nil {
				return nil, err
			}

			val, err := nestedValue(x, fields[1])
			if err != nil {
				return nil, err
			}

			out[key] = val
		}

		return out, nil
	}

	for _, f := range n.Fields() {
		x, err := nestedValue(m[f.Name()], f)
		if err != nil {
			return nil, err
		}

		m[f.Name()] = x
	}

	return m, nil
}

// Write serializes rows using the given Hive schema. Compression is an internal page codec for Parquet.
func (f *Format) Write(w io.Writer, columns []Column, rows [][]*string, compression string) error {
	if f.Kind != Parquet {
		return f.writeText(w, columns, rows, compression)
	}

	group := parquet.Group{}
	for _, c := range columns {
		n, err := nodeFor(c.Type)
		if err != nil {
			return err
		}

		group[c.Name] = parquet.Optional(n)
	}

	schema := parquet.NewSchema("table", group)
	c, err := codec(compression)
	if err != nil {
		return err
	}

	writer := parquet.NewWriter(w, schema, parquet.Compression(c))
	for _, r := range rows {
		object := map[string]any{}
		for i, col := range columns {
			if i >= len(r) || r[i] == nil {
				continue
			}

			v, err := parquetValue(*r[i], group[col.Name])
			if err != nil {
				return fmt.Errorf("column %s: %w", col.Name, err)
			}

			object[col.Name] = v
		}

		var row parquet.Row
		index := 0
		encodeRow(schema, object, 0, 0, 0, &index, &row)
		slices.SortStableFunc(row, func(a, b parquet.Value) int { return a.Column() - b.Column() })
		if _, err := writer.WriteRows([]parquet.Row{row}); err != nil {
			return err
		}
	}

	return writer.Close()
}

// encodeRow preserves definition and repetition levels, including null nested elements.
func encodeRow(n parquet.Node, v any, rep, def, depth int, index *int, row *parquet.Row) {
	if n.Optional() {
		if v == nil {
			emitNull(n, rep, def, index, row)
			return
		}

		def++
		n = parquet.Required(n)
	}

	if n.Repeated() {
		a, _ := v.([]any)
		if len(a) == 0 {
			emitNull(n, rep, def, index, row)
			return
		}

		depth++
		def++
		start := *index
		end := start
		for i, x := range a {
			*index = start
			r := rep
			if i > 0 {
				r = depth
			}

			encodeRow(parquet.Required(n), x, r, def, depth, index, row)
			end = *index
		}

		*index = end
		return
	}

	if n.Leaf() {
		value := parquet.ValueOf(v)
		if n.Type().Kind() == parquet.FixedLenByteArray {
			value = parquet.FixedLenByteArrayValue(v.([]byte))
		}

		*row = append(*row, value.Level(rep, def, *index))
		*index++
		return
	}

	if logicalType[*format.ListType](n) != nil {
		a, _ := v.([]any)
		items := make([]any, len(a))
		for i, x := range a {
			items[i] = map[string]any{"element": x}
		}

		v = map[string]any{"list": items}
	}

	if logicalType[*format.MapType](n) != nil {
		m, _ := v.(map[any]any)
		items := make([]any, 0, len(m))
		for k, x := range m {
			items = append(items, map[string]any{"key": k, "value": x})
		}

		v = map[string]any{"key_value": items}
	}

	obj, _ := v.(map[string]any)
	for _, f := range n.Fields() {
		encodeRow(f, obj[f.Name()], rep, def, depth, index, row)
	}
}

func emitNull(n parquet.Node, rep, def int, index *int, row *parquet.Row) {
	if n.Leaf() {
		*row = append(*row, parquet.NullValue().Level(rep, def, *index))
		*index++
		return
	}

	for _, f := range n.Fields() {
		emitNull(f, rep, def, index, row)
	}
}

func leafCount(n parquet.Node) int {
	if n.Leaf() {
		return 1
	}

	count := 0
	for _, f := range n.Fields() {
		count += leafCount(f)
	}

	return count
}

func decodeNode(n parquet.Node, cols [][]parquet.Value, def, depth int) any {
	if len(cols) == 0 || len(cols[0]) == 0 {
		return nil
	}

	if n.Optional() {
		def++
		if cols[0][0].DefinitionLevel() < def {
			return nil
		}

		n = parquet.Required(n)
	}

	if n.Repeated() {
		def++
		depth++
		if cols[0][0].DefinitionLevel() < def {
			return []any{}
		}

		segments := make([][][]parquet.Value, len(cols))
		count := 0
		for i, values := range cols {
			start := 0
			for j := 1; j <= len(values); j++ {
				if j == len(values) || values[j].RepetitionLevel() <= depth {
					segments[i] = append(segments[i], values[start:j])
					start = j
				}
			}

			if len(segments[i]) > count {
				count = len(segments[i])
			}
		}

		out := make([]any, count)
		for j := range count {
			part := make([][]parquet.Value, len(cols))
			for i := range cols {
				if j < len(segments[i]) {
					part[i] = segments[i][j]
				}
			}

			out[j] = decodeNode(parquet.Required(n), part, def, depth)
		}

		return out
	}

	if n.Leaf() {
		v := cols[0][0]
		if v.IsNull() {
			return nil
		}

		switch v.Kind() {
		case parquet.Boolean:
			return v.Boolean()
		case parquet.Int32:
			return v.Int32()
		case parquet.Int64:
			return v.Int64()
		case parquet.Float:
			return v.Float()
		case parquet.Double:
			return v.Double()
		default:
			return string(v.ByteArray())
		}
	}

	obj := map[string]any{}
	offset := 0
	for _, f := range n.Fields() {
		count := leafCount(f)
		obj[f.Name()] = decodeNode(f, cols[offset:offset+count], def, depth)
		offset += count
	}

	if logicalType[*format.ListType](n) != nil {
		items, _ := obj["list"].([]any)
		out := make([]any, len(items))
		for i, x := range items {
			if m, ok := x.(map[string]any); ok {
				out[i] = m[n.Fields()[0].Fields()[0].Name()]
			}
		}

		return out
	}

	if logicalType[*format.MapType](n) != nil {
		items, _ := obj["key_value"].([]any)
		out := map[string]any{}
		for _, x := range items {
			m, _ := x.(map[string]any)
			out[fmt.Sprint(m["key"])] = m["value"]
		}

		return out
	}

	return obj
}

func parseDecimal(s string, precision, scale int) (*big.Int, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("invalid decimal %q", s)
	}

	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)))
	if !r.IsInt() {
		return nil, fmt.Errorf("decimal exceeds scale %d", scale)
	}

	n := r.Num()
	if len(new(big.Int).Abs(n).String()) > precision {
		return nil, fmt.Errorf("decimal exceeds precision %d", precision)
	}

	return n, nil
}

func decimalText(v any, scale int) string {
	n := new(big.Int)
	switch x := v.(type) {
	case string:
		b := []byte(x)
		n.SetBytes(b)
		if len(b) > 0 && b[0]&128 != 0 {
			n.Sub(n, new(big.Int).Lsh(big.NewInt(1), uint(len(b)*8)))
		}
	case []byte:
		n.SetBytes(x)
		if len(x) > 0 && x[0]&128 != 0 {
			n.Sub(n, new(big.Int).Lsh(big.NewInt(1), uint(len(x)*8)))
		}
	default:
		n.SetInt64(int64Number(v))
	}

	sign := ""
	if n.Sign() < 0 {
		sign = "-"
		n.Abs(n)
	}

	text := n.String()
	for len(text) <= scale {
		text = "0" + text
	}

	if scale > 0 {
		text = text[:len(text)-scale] + "." + text[len(text)-scale:]
	}

	return sign + text
}

func normalizeNested(v any, n parquet.Node) any {
	if v == nil {
		return nil
	}

	if n.Leaf() {
		kind := hiveType(n)
		if kind == hiveDate || kind == hiveTimestamp || strings.HasPrefix(kind, "decimal") {
			text, _ := parquetText(v, n)
			return text
		}

		return v
	}

	if logicalType[*format.ListType](n) != nil {
		a, _ := v.([]any)
		out := make([]any, len(a))
		for i, x := range a {
			out[i] = normalizeNested(x, n.Fields()[0].Fields()[0])
		}

		return out
	}

	m, _ := v.(map[string]any)
	out := map[string]any{}
	if logicalType[*format.MapType](n) != nil {
		for k, x := range m {
			out[k] = normalizeNested(x, n.Fields()[0].Fields()[1])
		}

		return out
	}

	for _, f := range n.Fields() {
		out[f.Name()] = normalizeNested(m[f.Name()], f)
	}

	return out
}
