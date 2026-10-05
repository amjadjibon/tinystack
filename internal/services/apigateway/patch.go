package apigateway

import (
	"reflect"
	"slices"
	"strconv"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	opAdd     = "add"
	opRemove  = "remove"
	opReplace = "replace"
)

// pathSegments splits a patch path such as "/binaryMediaTypes/image~1png"
// into its unescaped segments ("~1" is "/" and "~0" is "~").
func pathSegments(path string) []string {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return nil
	}

	segments := strings.Split(trimmed, "/")
	for i, s := range segments {
		segments[i] = strings.NewReplacer("~1", "/", "~0", "~").Replace(s)
	}

	return segments
}

// patch applies add, remove, and replace operations to v, a pointer to a
// generated struct, through its JSON member names. Only members listed in
// fields may be changed. Scalars take the operation's value, parsed to the
// member's type; maps take "/<member>/<key>"; lists take the element as the
// last segment or the value, and replace the element "/<member>/<index>" or
// "/<member>/<old element>" names.
func patch(v any, ops []api.PatchOperation, fields ...string) error {
	for _, op := range ops {
		path := common.Value(op.Path)
		segments := pathSegments(path)
		if len(segments) == 0 || !slices.Contains(fields, segments[0]) {
			return badRequest("Invalid patch path '%s' specified for op '%s'. Must be one of: [%s]", path,
				common.Value(op.Op), strings.Join(fields, ", "))
		}

		switch common.Value(op.Op) {
		case opAdd, opRemove, opReplace:
		default:
			return badRequest("Invalid patch operation specified. Must be 'add'|'remove'|'replace'")
		}

		if err := apply(reflect.ValueOf(v).Elem(), common.Value(op.Op), segments, op.Value, path); err != nil {
			return err
		}
	}

	return nil
}

// field returns the struct field whose JSON name is name.
func field(v reflect.Value, name string) (reflect.Value, bool) {
	t := v.Type()
	for i := range t.NumField() {
		tag, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if tag == name {
			return v.Field(i), true
		}
	}

	return reflect.Value{}, false
}

func apply(v reflect.Value, op string, segments []string, raw *string, path string) error {
	invalid := badRequest("Invalid patch path '%s' specified for op '%s'", path, op)
	f, ok := field(v, segments[0])
	if !ok {
		return invalid
	}

	rest := segments[1:]
	switch f.Kind() {
	case reflect.Pointer:
		elem := f.Type().Elem()
		if elem.Kind() == reflect.Struct {
			switch {
			case len(rest) == 0 && op == opRemove:
				f.Set(reflect.Zero(f.Type()))
				return nil
			case len(rest) == 0:
				return invalid
			case f.IsNil() && op == opRemove:
				return nil
			case f.IsNil():
				f.Set(reflect.New(elem))
			}

			return apply(f.Elem(), op, rest, raw, path)
		}

		if len(rest) != 0 {
			return invalid
		}

		if op == opRemove {
			f.Set(reflect.Zero(f.Type()))
			return nil
		}

		parsed, err := scalar(elem, raw, path)
		if err != nil {
			return err
		}

		ptr := reflect.New(elem)
		ptr.Elem().Set(parsed)
		f.Set(ptr)
	case reflect.Map:
		if len(rest) != 1 {
			return invalid
		}

		if f.IsNil() {
			f.Set(reflect.MakeMap(f.Type()))
		}

		key := reflect.ValueOf(rest[0])
		if op == opRemove {
			f.SetMapIndex(key, reflect.Value{})
			return nil
		}

		parsed, err := scalar(f.Type().Elem(), raw, path)
		if err != nil {
			return err
		}

		f.SetMapIndex(key, parsed)
	case reflect.Slice:
		return applyList(f, op, rest, raw, invalid)
	default:
		return invalid
	}

	return nil
}

// applyList changes a list of strings.
func applyList(f reflect.Value, op string, rest []string, raw *string, invalid error) error {
	list, _ := f.Interface().([]string)
	item := common.Value(raw)
	if len(rest) == 1 {
		item = rest[0]
	}

	if len(rest) > 1 {
		return invalid
	}

	switch op {
	case opAdd:
		if !slices.Contains(list, item) {
			list = append(list, item)
		}
	case opRemove:
		list = slices.DeleteFunc(list, func(s string) bool { return s == item })
	case opReplace:
		// "/<member>/<index>" or "/<member>/<old element>" names the element.
		index, err := strconv.Atoi(item)
		if err != nil || index < 0 || index >= len(list) {
			index = slices.Index(list, item)
		}

		switch {
		case len(rest) == 0:
			list = []string{common.Value(raw)}
		case index < 0:
			return invalid
		default:
			list[index] = common.Value(raw)
		}
	}

	f.Set(reflect.ValueOf(list))
	return nil
}

// scalar parses a patch value as t, a string, bool, or number.
func scalar(t reflect.Type, raw *string, path string) (reflect.Value, error) {
	s := common.Value(raw)
	var parsed any
	var err error
	switch t.Kind() {
	case reflect.String:
		parsed = s
	case reflect.Bool:
		parsed, err = strconv.ParseBool(s)
	case reflect.Int64:
		parsed, err = strconv.ParseInt(s, 10, 64)
	case reflect.Float64:
		parsed, err = strconv.ParseFloat(s, 64)
	default:
		return reflect.Value{}, badRequest("Invalid patch path %s", path)
	}

	if err != nil {
		return reflect.Value{}, badRequest("Invalid value '%s' for %s", s, path)
	}

	return reflect.ValueOf(parsed), nil
}
