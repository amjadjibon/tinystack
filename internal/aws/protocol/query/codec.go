// Package query implements the AWS Query protocol from generated `query` struct
// tags, and EC2 Query from `ec2` tags.
//
// A `query` tag names the member's query key and XML element, followed by
// options: "flat" for flattened collections, and "member=", "key=", or "value="
// for non-default list member and map entry names. An `ec2` tag names the query
// key and then the XML element, which differ in EC2 Query, followed by the same
// options; EC2 Query lists are always flat in requests and wrapped in
// responses. Timestamps are time.Time values written as ISO 8601 date-times.
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
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

var timeType = reflect.TypeFor[time.Time]()

// Struct tag keys of the two dialects.
const (
	queryKey = "query"
	ec2Key   = "ec2"
)

// tag is a member's wire names: name is its query key and element its XML
// element.
type tag struct {
	name, element, member, key, value string
	flat                              bool
}

func parseTag(raw, dialect string) tag {
	parts := strings.Split(raw, ",")
	t := tag{name: parts[0], element: parts[0], member: "member", key: "key", value: "value"}
	options := parts[1:]
	if dialect == ec2Key && len(options) > 0 {
		t.element, options = options[0], options[1:]
	}

	for _, opt := range options {
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
	params  url.Values
	used    map[string]bool
	dialect string
}

// Decode fills input from AWS Query parameters, rejecting any parameter it does not consume.
func Decode(params url.Values, input any) error {
	return decode(params, input, queryKey)
}

// DecodeEC2 fills input from EC2 Query parameters, rejecting any parameter it
// does not consume.
func DecodeEC2(params url.Values, input any) error {
	return decode(params, input, ec2Key)
}

func decode(params url.Values, input any, dialect string) error {
	d := decoder{params: params, used: map[string]bool{"Action": true, "Version": true}, dialect: dialect}
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
		raw, ok := t.Field(i).Tag.Lookup(d.dialect)
		if !ok {
			return fmt.Errorf("query: %s.%s has no %s tag", t.Name(), t.Field(i).Name, d.dialect)
		}

		tg := parseTag(raw, d.dialect)
		if err := d.field(v.Field(i), join(prefix, tg.name), &tg); err != nil {
			return err
		}
	}

	return nil
}

func (d *decoder) field(v reflect.Value, key string, tg *tag) error {
	switch v.Kind() {
	case reflect.Pointer:
		elem := v.Type().Elem()
		_, set := d.params[key]
		if elem.Kind() == reflect.Struct && elem != timeType {
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
		if v.Type() == timeType {
			return d.scalar(v, key)
		}

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

func (d *decoder) list(v reflect.Value, key string, tg *tag) error {
	base := key
	if !tg.flat && d.dialect != ec2Key {
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
		if guardErr := d.field(s.Index(i), base+"."+strconv.Itoa(n), &tag{}); guardErr != nil {
			return guardErr
		}
	}

	v.Set(s)
	return nil
}

func (d *decoder) mapping(v reflect.Value, key string, tg *tag) error {
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
		if guardErr := d.field(value, valueKey, &tag{}); guardErr != nil {
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
	case reflect.Struct:
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return invalid("Parameter %s must be an ISO 8601 date-time", key)
		}

		v.Set(reflect.ValueOf(ts))
	default:
		return fmt.Errorf("query: unsupported kind %s for %s", v.Kind(), key)
	}

	return nil
}

func open(b *bytes.Buffer, name string) {
	b.WriteString("<")
	b.WriteString(name)
	b.WriteString(">")
}

func closeTag(b *bytes.Buffer, name string) {
	b.WriteString("</")
	b.WriteString(name)
	b.WriteString(">")
}

func text(b *bytes.Buffer, s string) { _ = xml.EscapeText(b, []byte(s)) }

func namespaced(b *bytes.Buffer, name, namespace string) {
	b.WriteString("<")
	b.WriteString(name)
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
		encodeStruct(&b, v, queryKey)
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
	b.WriteString("<Error><Type>")
	b.WriteString(e.Fault())
	b.WriteString("</Type><Code>")
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

// WriteEC2 encodes a successful EC2 Query response: the output's members sit
// directly under the <Operation>Response root, after its requestId. A nil
// output, used for Unit outputs, writes only the requestId.
func WriteEC2(w http.ResponseWriter, status int, operation, namespace, requestID string, output any) {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	namespaced(&b, operation+"Response", namespace)
	b.WriteString("<requestId>")
	text(&b, requestID)
	b.WriteString("</requestId>")
	v := reflect.ValueOf(output)
	for v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}

	if v.Kind() == reflect.Struct {
		encodeStruct(&b, v, ec2Key)
	}

	closeTag(&b, operation+"Response")
	w.Header().Set("Content-Type", "text/xml;charset=UTF-8")
	w.WriteHeader(status)
	_, _ = w.Write(b.Bytes())
}

// WriteEC2Error encodes the EC2 Query error envelope, which has no namespace.
func WriteEC2Error(w http.ResponseWriter, requestID string, e *awserror.Error) {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString("<Response><Errors><Error><Code>")
	text(&b, e.Code)
	b.WriteString("</Code><Message>")
	text(&b, e.Message)
	b.WriteString("</Message></Error></Errors><RequestID>")
	text(&b, requestID)
	b.WriteString("</RequestID></Response>")
	w.Header().Set("Content-Type", "text/xml;charset=UTF-8")
	w.WriteHeader(e.StatusCode)
	_, _ = w.Write(b.Bytes())
}

func encodeStruct(b *bytes.Buffer, v reflect.Value, dialect string) {
	t := v.Type()
	for i := range t.NumField() {
		tg := parseTag(t.Field(i).Tag.Get(dialect), dialect)
		encodeField(b, &tg, v.Field(i), dialect)
	}
}

func encodeField(b *bytes.Buffer, tg *tag, v reflect.Value, dialect string) {
	if (v.Kind() == reflect.Pointer || v.Kind() == reflect.Map || v.Kind() == reflect.Slice) && v.IsNil() {
		return
	}

	switch {
	case v.Kind() == reflect.Pointer:
		encodeField(b, tg, v.Elem(), dialect)
	case v.Kind() == reflect.Slice && v.Type().Elem().Kind() != reflect.Uint8:
		if !tg.flat {
			open(b, tg.element)
		}

		for i := range v.Len() {
			name := tg.member
			if tg.flat {
				name = tg.element
			}

			element(b, name, v.Index(i), dialect)
		}

		if !tg.flat {
			closeTag(b, tg.element)
		}
	case v.Kind() == reflect.Map:
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		entry := "entry"
		if tg.flat {
			entry = tg.element
		} else {
			open(b, tg.element)
		}

		for _, k := range keys {
			open(b, entry)
			element(b, tg.key, k, dialect)
			element(b, tg.value, v.MapIndex(k), dialect)
			closeTag(b, entry)
		}

		if !tg.flat {
			closeTag(b, tg.element)
		}
	default:
		element(b, tg.element, v, dialect)
	}
}

func element(b *bytes.Buffer, name string, v reflect.Value, dialect string) {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}

		v = v.Elem()
	}

	open(b, name)
	switch v.Kind() {
	case reflect.Struct:
		if ts, ok := v.Interface().(time.Time); ok {
			b.WriteString(ts.UTC().Format(time.RFC3339))
			break
		}

		encodeStruct(b, v, dialect)
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
