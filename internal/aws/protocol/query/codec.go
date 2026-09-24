// Package query implements the AWS Query protocol from generated `query` struct tags.
//
// A tag names the member's query key and XML element, followed by options:
// "flat" for flattened collections, and "member=", "key=", or "value=" for
// non-default list member and map entry names.
package query

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

type tag struct {
	name, member, key, value string
	flat                     bool
}

func parseTag(raw string) tag {
	parts := strings.Split(raw, ",")
	t := tag{name: parts[0], member: "member", key: "key", value: "value"}
	for _, opt := range parts[1:] {
		k, v, _ := strings.Cut(opt, "=")
		switch k {
		case "flat":
			t.flat = true
		case "member":
			t.member = v
		case "key":
			t.key = v
		case "value":
			t.value = v
		}
	}

	return t
}

func invalid(format string, args ...any) error {
	return awserror.New("InvalidParameterValue", fmt.Sprintf(format, args...))
}

type decoder struct {
	params url.Values
	used   map[string]bool
}

// Decode fills input from AWS Query parameters, rejecting any parameter it does not consume.
func Decode(params url.Values, input any) error {
	d := decoder{params: params, used: map[string]bool{"Action": true, "Version": true}}
	if err := d.structure(reflect.ValueOf(input).Elem(), ""); err != nil {
		return err
	}

	unknown := make([]string, 0)
	for k := range params {
		if !d.used[k] {
			unknown = append(unknown, k)
		}
	}

	if len(unknown) > 0 {
		sort.Strings(unknown)
		return invalid("Unknown or unsupported parameter %s", unknown[0])
	}

	return nil
}

func join(prefix, name string) string {
	if prefix == "" {
		return name
	}

	return prefix + "." + name
}

// nested reports whether any parameter lies below key.
func (d *decoder) nested(key string) bool {
	for k := range d.params {
		if strings.HasPrefix(k, key+".") {
			return true
		}
	}

	return false
}

func (d *decoder) structure(v reflect.Value, prefix string) error {
	t := v.Type()
	for i := range t.NumField() {
		raw, ok := t.Field(i).Tag.Lookup("query")
		if !ok {
			return fmt.Errorf("query: %s.%s has no query tag", t.Name(), t.Field(i).Name)
		}

		tg := parseTag(raw)
		if err := d.field(v.Field(i), join(prefix, tg.name), tg); err != nil {
			return err
		}
	}

	return nil
}

func (d *decoder) field(v reflect.Value, key string, tg tag) error {
	switch v.Kind() {
	case reflect.Pointer:
		elem := v.Type().Elem()
		_, set := d.params[key]
		if elem.Kind() == reflect.Struct {
			set = d.nested(key)
		}

		if !set {
			return nil
		}

		p := reflect.New(elem)
		if err := d.field(p.Elem(), key, tg); err != nil {
			return err
		}

		v.Set(p)
		return nil
	case reflect.Struct:
		return d.structure(v, key)
	case reflect.Slice:
		if v.Type().Elem().Kind() != reflect.Uint8 {
			return d.list(v, key, tg)
		}
	case reflect.Map:
		return d.mapping(v, key, tg)
	}

	return d.scalar(v, key)
}

// indices returns the consecutive 1-based positions below base.
func (d *decoder) indices(base string) ([]int, error) {
	seen := map[int]bool{}
	for k := range d.params {
		rest, ok := strings.CutPrefix(k, base+".")
		if !ok {
			continue
		}

		segment, _, _ := strings.Cut(rest, ".")
		if n, err := strconv.Atoi(segment); err == nil && n > 0 && strconv.Itoa(n) == segment {
			seen[n] = true
		}
	}

	for n := 1; n <= len(seen); n++ {
		if !seen[n] {
			return nil, invalid("Members of %s must be numbered consecutively from 1", base)
		}
	}

	out := make([]int, len(seen))
	for i := range out {
		out[i] = i + 1
	}

	return out, nil
}

// empty consumes the "Name=" form AWS Query uses for an empty collection.
func (d *decoder) empty(key string) bool {
	values, ok := d.params[key]
	if ok && len(values) == 1 && values[0] == "" {
		d.used[key] = true
		return true
	}

	return false
}

func (d *decoder) list(v reflect.Value, key string, tg tag) error {
	base := key
	if !tg.flat {
		base = key + "." + tg.member
	}

	isEmpty := d.empty(key)
	idx, err := d.indices(base)
	if err != nil {
		return err
	}

	if len(idx) == 0 {
		if isEmpty {
			v.Set(reflect.MakeSlice(v.Type(), 0, 0))
		}

		return nil
	}

	s := reflect.MakeSlice(v.Type(), len(idx), len(idx))
	for i, n := range idx {
		if guardErr := d.field(s.Index(i), base+"."+strconv.Itoa(n), tag{}); guardErr != nil {
			return guardErr
		}
	}

	v.Set(s)
	return nil
}

func (d *decoder) mapping(v reflect.Value, key string, tg tag) error {
	base := key
	if !tg.flat {
		base = key + ".entry"
	}

	isEmpty := d.empty(key)
	idx, err := d.indices(base)
	if err != nil {
		return err
	}

	if len(idx) == 0 && !isEmpty {
		return nil
	}

	m := reflect.MakeMapWithSize(v.Type(), len(idx))
	for _, n := range idx {
		entry := base + "." + strconv.Itoa(n)
		names, ok := d.params[entry+"."+tg.key]
		if !ok || len(names) != 1 {
			return invalid("%s requires exactly one %s", entry, tg.key)
		}

		d.used[entry+"."+tg.key] = true
		name := reflect.ValueOf(names[0])
		if m.MapIndex(name).IsValid() {
			return invalid("Duplicate %s %q in %s", tg.key, names[0], key)
		}

		valueKey := entry + "." + tg.value
		if _, set := d.params[valueKey]; !set && !d.nested(valueKey) {
			return invalid("%s requires a %s", entry, tg.value)
		}

		value := reflect.New(v.Type().Elem()).Elem()
		if guardErr := d.field(value, valueKey, tag{}); guardErr != nil {
			return guardErr
		}

		m.SetMapIndex(name, value)
	}

	v.Set(m)
	return nil
}

func (d *decoder) scalar(v reflect.Value, key string) error {
	values, ok := d.params[key]
	if !ok {
		return nil
	}

	if len(values) != 1 {
		return invalid("Parameter %s must be given once", key)
	}

	d.used[key] = true
	s := values[0]
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		if s != "true" && s != "false" {
			return invalid("Parameter %s must be true or false", key)
		}

		v.SetBool(s == "true")
	case reflect.Int64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return invalid("Parameter %s must be an integer", key)
		}

		v.SetInt(n)
	case reflect.Float64:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return invalid("Parameter %s must be a number", key)
		}

		v.SetFloat(f)
	case reflect.Slice:
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return invalid("Parameter %s must be base64 encoded", key)
		}

		v.SetBytes(b)
	default:
		return fmt.Errorf("query: unsupported kind %s for %s", v.Kind(), key)
	}

	return nil
}

func open(b *bytes.Buffer, name string)     { b.WriteString("<" + name + ">") }
func closeTag(b *bytes.Buffer, name string) { b.WriteString("</" + name + ">") }

func text(b *bytes.Buffer, s string) { _ = xml.EscapeText(b, []byte(s)) }

func namespaced(b *bytes.Buffer, name, namespace string) {
	b.WriteString("<" + name)
	if namespace != "" {
		b.WriteString(` xmlns="`)
		text(b, namespace)
		b.WriteString(`"`)
	}

	b.WriteString(">")
}

func metadata(b *bytes.Buffer, requestID string) {
	b.WriteString("<RequestId>")
	text(b, requestID)
	b.WriteString("</RequestId>")
}

// Write encodes a successful operation response. A nil output, used for Unit
// outputs, writes no Result element.
func Write(w http.ResponseWriter, status int, operation, namespace, requestID string, output any) {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	namespaced(&b, operation+"Response", namespace)
	v := reflect.ValueOf(output)
	for v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}

	if v.Kind() == reflect.Struct {
		open(&b, operation+"Result")
		encodeStruct(&b, v)
		closeTag(&b, operation+"Result")
	}

	b.WriteString("<ResponseMetadata>")
	metadata(&b, requestID)
	b.WriteString("</ResponseMetadata>")
	closeTag(&b, operation+"Response")
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	_, _ = w.Write(b.Bytes())
}

// WriteError encodes the AWS Query error envelope.
func WriteError(w http.ResponseWriter, namespace, requestID string, e *awserror.Error) {
	code := e.QueryCode
	if code == "" {
		code = e.Code
	}

	var b bytes.Buffer
	b.WriteString(xml.Header)
	namespaced(&b, "ErrorResponse", namespace)
	b.WriteString("<Error><Type>" + e.Fault() + "</Type><Code>")
	text(&b, code)
	b.WriteString("</Code><Message>")
	text(&b, e.Message)
	b.WriteString("</Message><Detail/></Error>")
	metadata(&b, requestID)
	closeTag(&b, "ErrorResponse")
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(e.StatusCode)
	_, _ = w.Write(b.Bytes())
}

func encodeStruct(b *bytes.Buffer, v reflect.Value) {
	t := v.Type()
	for i := range t.NumField() {
		encodeField(b, parseTag(t.Field(i).Tag.Get("query")), v.Field(i))
	}
}

func encodeField(b *bytes.Buffer, tg tag, v reflect.Value) {
	if (v.Kind() == reflect.Pointer || v.Kind() == reflect.Map || v.Kind() == reflect.Slice) && v.IsNil() {
		return
	}

	switch {
	case v.Kind() == reflect.Pointer:
		encodeField(b, tg, v.Elem())
	case v.Kind() == reflect.Slice && v.Type().Elem().Kind() != reflect.Uint8:
		if !tg.flat {
			open(b, tg.name)
		}

		for i := range v.Len() {
			name := tg.member
			if tg.flat {
				name = tg.name
			}

			element(b, name, v.Index(i))
		}

		if !tg.flat {
			closeTag(b, tg.name)
		}
	case v.Kind() == reflect.Map:
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		entry := "entry"
		if tg.flat {
			entry = tg.name
		} else {
			open(b, tg.name)
		}

		for _, k := range keys {
			open(b, entry)
			element(b, tg.key, k)
			element(b, tg.value, v.MapIndex(k))
			closeTag(b, entry)
		}

		if !tg.flat {
			closeTag(b, tg.name)
		}
	default:
		element(b, tg.name, v)
	}
}

func element(b *bytes.Buffer, name string, v reflect.Value) {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}

		v = v.Elem()
	}

	open(b, name)
	switch v.Kind() {
	case reflect.Struct:
		encodeStruct(b, v)
	case reflect.String:
		text(b, v.String())
	case reflect.Bool:
		b.WriteString(strconv.FormatBool(v.Bool()))
	case reflect.Int64:
		b.WriteString(strconv.FormatInt(v.Int(), 10))
	case reflect.Float64:
		b.WriteString(strconv.FormatFloat(v.Float(), 'f', -1, 64))
	case reflect.Slice:
		b.WriteString(base64.StdEncoding.EncodeToString(v.Bytes()))
	}

	closeTag(b, name)
}
