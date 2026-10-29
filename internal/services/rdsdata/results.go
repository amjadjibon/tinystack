package rdsdata

import (
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rdsdata"
)

func resultField(value any, typeName string) (api.Field, error) {
	if strings.HasPrefix(typeName, "_") || strings.HasSuffix(typeName, "[]") {
		return api.Field{}, awserror.New("UnsupportedResultException", "Array results are unsupported")
	}

	if value == nil {
		return api.Field{IsNull: new(true)}, nil
	}

	switch v := value.(type) {
	case bool:
		return api.Field{BooleanValue: new(v)}, nil
	case int64:
		return api.Field{LongValue: new(v)}, nil
	case float64:
		return api.Field{DoubleValue: new(v)}, nil
	case time.Time:
		format := "2006-01-02 15:04:05.999999"
		switch strings.ToUpper(typeName) {
		case hintDate:
			format = "2006-01-02"
		case hintTime, "TIMETZ":
			format = "15:04:05.999999"
		}

		return api.Field{StringValue: new(v.Format(format))}, nil
	case []byte:
		switch strings.ToUpper(typeName) {
		case typeBytea, "BLOB", "BINARY", "VARBINARY", "LONGBLOB", "MEDIUMBLOB", "TINYBLOB":
			return api.Field{BlobValue: new(v)}, nil
		default:
			return textField(string(v), typeName)
		}

	case string:
		return textField(v, typeName)
	default:
		return api.Field{}, awserror.New(
			"UnsupportedResultException",
			fmt.Sprintf("Unsupported SQL result type %T", value),
		)
	}
}

func textField(value, typeName string) (api.Field, error) {
	switch strings.ToUpper(typeName) {
	case "BOOL", typeBoolean:
		v, err := strconv.ParseBool(value)
		if err != nil {
			return api.Field{}, sqlError(err)
		}

		return api.Field{BooleanValue: new(v)}, nil
	case "INT2", "INT4", "INT8", "SMALLINT", "INT", "INTEGER", typeBigInt, "TINYINT", "MEDIUMINT", "YEAR":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return api.Field{}, sqlError(err)
		}

		return api.Field{LongValue: new(v)}, nil
	case "FLOAT4", "FLOAT8", "FLOAT", "DOUBLE", "REAL":
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return api.Field{}, sqlError(err)
		}

		return api.Field{DoubleValue: new(v)}, nil
	}

	return api.Field{StringValue: new(value)}, nil
}

func metadata(c *sql.ColumnType) api.ColumnMetadata {
	typ := strings.ToUpper(c.DatabaseTypeName())
	code := int64(12)
	switch typ {
	case "BOOL", typeBoolean:
		code = 16
	case "INT2", "SMALLINT":
		code = 5
	case "INT4", "INT", "INTEGER", "TINYINT", "MEDIUMINT", "YEAR":
		code = 4
	case "INT8", typeBigInt:
		code = -5
	case "FLOAT4", "REAL":
		code = 7
	case "FLOAT8", "FLOAT", "DOUBLE":
		code = 8
	case "NUMERIC", hintDecimal:
		code = 3
	case hintDate:
		code = 91
	case hintTime, "TIMETZ":
		code = 92
	case hintTimestamp, "TIMESTAMPTZ", "DATETIME":
		code = 93
	case typeBytea, "BLOB", "BINARY", "VARBINARY", "LONGBLOB", "MEDIUMBLOB", "TINYBLOB":
		code = -3
	case "UUID", hintJSON, "JSONB":
		code = 1111
	}

	nullable := int64(2)
	if yes, ok := c.Nullable(); ok {
		nullable = 0
		if yes {
			nullable = 1
		}
	}

	out := api.ColumnMetadata{
		Name:     new(c.Name()),
		Label:    new(c.Name()),
		TypeName: new(typ),
		Type:     new(code),
		Nullable: new(nullable),
		IsSigned: new(code == 4 || code == 5 || code == -5 || code == 3 || code == 7 || code == 8),
	}

	// The model's precision and scale are 32-bit; drivers report math.MaxInt64 for sizes that are not fixed
	// (MySQL DOUBLE), and those are left out.
	if precision, scale, ok := c.DecimalSize(); ok && precision <= math.MaxInt32 && scale <= math.MaxInt32 {
		out.Precision, out.Scale = new(precision), new(scale)
	}

	return out
}
