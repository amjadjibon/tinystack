package catalog

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

// Caller calls the operations of registered services in-process, as Step
// Functions' AWS SDK integrations do. Parameters and results are JSON-like
// values keyed by the SDK's member names (TableName, FunctionName), which are
// the generated types' Go field names; wire names and HTTP bindings play no
// part.
type Caller struct {
	services map[string]*Service
	// lenient takes text for numbers and booleans, and numbers and booleans
	// for text, as CloudFormation templates write them.
	lenient bool
}

// NewCaller returns a Caller for services, found by their names.
func NewCaller(services ...Service) *Caller {
	c := &Caller{services: map[string]*Service{}}
	for i := range services {
		c.services[services[i].Name] = &services[i]
	}

	return c
}

// Lenient returns a Caller for the same services that converts scalars as
// CloudFormation does: "5" sets a number, "true" a boolean, and 5 or true a
// string.
func (c *Caller) Lenient() *Caller { return &Caller{services: c.services, lenient: true} }

// Call runs operation of the service named service with params in ctx, which
// carries the caller's request metadata. It reports an unknown service or
// operation, and parameters that do not fit the operation's input, as
// awserror values with the "UnknownOperationException" and
// "ValidationException" codes.
func (c *Caller) Call(ctx context.Context, service, operation string, params map[string]any) (map[string]any, error) {
	s, ok := c.services[service]
	if !ok {
		return nil, awserror.New("UnknownOperationException", "TinyStack does not implement the "+service+" service")
	}

	op, ok := s.Operations[operation]
	if !ok {
		return nil, awserror.New("UnknownOperationException",
			fmt.Sprintf("TinyStack does not implement %s's %s operation", service, operation))
	}

	input := op.NewInput()
	if err := (assigner{lenient: c.lenient}).assign(reflect.ValueOf(input).Elem(), params, operation); err != nil {
		return nil, awserror.Validation("%s", err.Error())
	}

	output, err := op.Invoke(ctx, input)
	if err != nil {
		return nil, err
	}

	out, _ := export(reflect.ValueOf(output)).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}

	return out, nil
}

var timeType = reflect.TypeFor[time.Time]()

// assigner sets generated values from JSON-like ones.
type assigner struct{ lenient bool }

// assign sets dst, a generated type, from a JSON-like value.
func (a assigner) assign(dst reflect.Value, src any, path string) error {
	if src == nil {
		return nil
	}

	mismatch := fmt.Errorf("%s: unexpected %T", path, src)
	switch dst.Kind() {
	case reflect.Pointer:
		elem := reflect.New(dst.Type().Elem())
		if err := a.assign(elem.Elem(), src, path); err != nil {
			return err
		}

		dst.Set(elem)
	case reflect.Struct:
		if dst.Type() == timeType {
			return assignTime(dst, src, mismatch)
		}

		fields, ok := src.(map[string]any)
		if !ok {
			return mismatch
		}

		for name, v := range fields {
			f := dst.FieldByNameFunc(func(field string) bool { return strings.EqualFold(field, name) })
			if !f.IsValid() {
				return fmt.Errorf("%s: TinyStack does not support the %s member", path, name)
			}

			if err := a.assign(f, v, path+"."+name); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if dst.Type().Elem().Kind() == reflect.Uint8 {
			return assignBlob(dst, src, mismatch)
		}

		list, ok := src.([]any)
		if !ok {
			return mismatch
		}

		out := reflect.MakeSlice(dst.Type(), len(list), len(list))
		for i, v := range list {
			if err := a.assign(out.Index(i), v, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}

		dst.Set(out)
	case reflect.Map:
		entries, ok := src.(map[string]any)
		if !ok {
			return mismatch
		}

		out := reflect.MakeMapWithSize(dst.Type(), len(entries))
		for k, v := range entries {
			elem := reflect.New(dst.Type().Elem()).Elem()
			if err := a.assign(elem, v, path+"."+k); err != nil {
				return err
			}

			out.SetMapIndex(reflect.ValueOf(k), elem)
		}

		dst.Set(out)
	default:
		return assignScalar(dst, a.convert(dst.Kind(), src), mismatch)
	}

	return nil
}

// convert turns a lenient caller's scalar into the kind dst holds, leaving
// anything it cannot convert for assignScalar to reject.
func (a assigner) convert(kind reflect.Kind, src any) any {
	if !a.lenient {
		return src
	}

	switch v := src.(type) {
	case string:
		switch kind {
		case reflect.Bool:
			if b, err := strconv.ParseBool(v); err == nil {
				return b
			}
		case reflect.Int64, reflect.Float64:
			if n, err := strconv.ParseFloat(v, 64); err == nil {
				return n
			}
		default:
		}
	case float64:
		if kind == reflect.String {
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
	case bool:
		if kind == reflect.String {
			return strconv.FormatBool(v)
		}
	}

	return src
}

func assignScalar(dst reflect.Value, src any, mismatch error) error {
	switch dst.Kind() {
	case reflect.String:
		s, ok := src.(string)
		if !ok {
			return mismatch
		}

		dst.SetString(s)
	case reflect.Bool:
		b, ok := src.(bool)
		if !ok {
			return mismatch
		}

		dst.SetBool(b)
	case reflect.Int64:
		n, ok := src.(float64)
		if !ok || n != float64(int64(n)) {
			return mismatch
		}

		dst.SetInt(int64(n))
	case reflect.Float64:
		n, ok := src.(float64)
		if !ok {
			return mismatch
		}

		dst.SetFloat(n)
	case reflect.Interface:
		dst.Set(reflect.ValueOf(src))
	default:
		return mismatch
	}

	return nil
}

// assignBlob takes a blob as text, or as the JSON of any other value, as
// Step Functions passes a Lambda payload.
func assignBlob(dst reflect.Value, src any, mismatch error) error {
	switch v := src.(type) {
	case string:
		dst.SetBytes([]byte(v))
	case map[string]any, []any, float64, bool:
		data, err := json.Marshal(v)
		if err != nil {
			return mismatch
		}

		dst.SetBytes(data)
	default:
		return mismatch
	}

	return nil
}

func assignTime(dst reflect.Value, src any, mismatch error) error {
	switch v := src.(type) {
	case string:
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return mismatch
		}

		dst.Set(reflect.ValueOf(t))
	case float64:
		dst.Set(reflect.ValueOf(time.UnixMilli(int64(v * 1000)).UTC()))
	default:
		return mismatch
	}

	return nil
}

// export renders a generated value as a JSON-like value keyed by Go field
// names, leaving out unset members. Blobs become text when they are UTF-8,
// and base64 otherwise.
func export(v reflect.Value) any {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}

		return export(v.Elem())
	case reflect.Struct:
		if v.Type() == timeType {
			t, _ := v.Interface().(time.Time)
			return t.UTC().Format(time.RFC3339Nano)
		}

		out := map[string]any{}
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if !f.IsExported() || f.Type.String() == "xml.Name" {
				continue
			}

			if e := export(v.Field(i)); e != nil {
				out[f.Name] = e
			}
		}

		return out
	case reflect.Slice:
		if v.IsNil() {
			return nil
		}

		if v.Type().Elem().Kind() == reflect.Uint8 {
			if b := v.Bytes(); utf8.Valid(b) {
				return string(b)
			}

			return base64.StdEncoding.EncodeToString(v.Bytes())
		}

		out := make([]any, v.Len())
		for i := range out {
			out[i] = export(v.Index(i))
		}

		return out
	case reflect.Map:
		if v.IsNil() {
			return nil
		}

		out := map[string]any{}
		for _, k := range v.MapKeys() {
			out[k.String()] = export(v.MapIndex(k))
		}

		return out
	case reflect.Int64:
		return float64(v.Int())
	default:
		return v.Interface()
	}
}
