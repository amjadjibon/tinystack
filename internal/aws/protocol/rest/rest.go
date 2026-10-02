// Package rest implements the HTTP bindings shared by the AWS REST protocols:
// catalog.MemberBinding places each member in the URI, headers, query string,
// payload, or status code. The restxml and restjson codecs supply the document
// format.
package rest

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

const (
	locationLabel    = "label"
	locationHeader   = "header"
	locationQuery    = "query"
	locationPrefix   = "prefix"
	locationPayload  = "payload"
	locationStatus   = "status"
	locationDocument = "document"
	unsupported      = "unsupported:"
	awsChunked       = "aws-chunked"
)

var timeType = reflect.TypeFor[time.Time]()

func invalid(format string, args ...any) error {
	return awserror.New("InvalidArgument", fmt.Sprintf(format, args...))
}

// NotImplemented reports a modeled feature TinyStack does not implement.
func NotImplemented(message string) error {
	return &awserror.Error{Code: "NotImplemented", Message: message, StatusCode: http.StatusNotImplemented}
}

// TooLarge reports a payload over the size limit.
func TooLarge() error {
	return &awserror.Error{
		Code:       "EntityTooLarge",
		Message:    "Your proposed upload exceeds the maximum allowed size",
		StatusCode: http.StatusBadRequest,
	}
}

// ReadBody reads a request payload of at most limit bytes. It decodes the
// aws-chunked encoding of SigV4 streaming uploads, removes aws-chunked from
// Content-Encoding, and copies chunk trailers (such as checksums) into the
// request headers.
func ReadBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	reader := http.MaxBytesReader(w, r.Body, limit)
	encodings := strings.Split(r.Header.Get("Content-Encoding"), ",")
	chunked := strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-")
	kept := encodings[:0]
	for _, encoding := range encodings {
		if encoding = strings.TrimSpace(encoding); encoding == awsChunked {
			chunked = true
		} else if encoding != "" {
			kept = append(kept, encoding)
		}
	}

	r.Header.Del("Content-Encoding")
	if len(kept) > 0 {
		r.Header.Set("Content-Encoding", strings.Join(kept, ","))
	}

	if chunked {
		return dechunk(bufio.NewReader(reader), r.Header)
	}

	body, err := io.ReadAll(reader)
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return nil, TooLarge()
	}

	return body, err
}

// dechunk decodes "size[;chunk-signature=...]\r\ndata\r\n" chunks up to the
// zero-length chunk, then reads "name:value" trailers into header.
func dechunk(r *bufio.Reader, header http.Header) ([]byte, error) {
	malformed := func(err error) error {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return TooLarge()
		}

		return invalid("Malformed aws-chunked request body")
	}

	var body bytes.Buffer
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, malformed(err)
		}

		sizeText, _, _ := strings.Cut(strings.TrimRight(line, "\r\n"), ";")
		size, err := strconv.ParseInt(strings.TrimSpace(sizeText), 16, 64)
		if err != nil || size < 0 {
			return nil, malformed(nil)
		}

		if size == 0 {
			break
		}

		if _, err = io.CopyN(&body, r, size); err != nil {
			return nil, malformed(err)
		}

		if crlf, crlfErr := r.ReadString('\n'); crlfErr != nil || strings.TrimRight(crlf, "\r\n") != "" {
			return nil, malformed(nil)
		}
	}

	for {
		line, err := r.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, malformed(err)
		}

		if line = strings.TrimRight(line, "\r\n"); line != "" {
			name, value, ok := strings.Cut(line, ":")
			if !ok {
				return nil, malformed(nil)
			}

			header.Set(strings.TrimSpace(name), strings.TrimSpace(value))
		}
		// A blank line or the end of the body ends the trailers.
		if line == "" || errors.Is(err, io.EOF) {
			return body.Bytes(), nil
		}
	}
}

// fields calls fn for each bound member of a generated structure.
func fields(v reflect.Value, bindings map[string]catalog.MemberBinding, fn func(reflect.Value, catalog.MemberBinding)) {
	t := v.Type()
	for i := range t.NumField() {
		if b, ok := bindings[t.Field(i).Name]; ok {
			fn(v.Field(i), b)
		}
	}
}

// Decode fills input from the request. labels holds the decoded URI labels and
// body the request payload; document decodes a document body or a structure
// payload. Modeled inputs the service does not implement fail.
func Decode(
	r *http.Request,
	b *catalog.Binding,
	labels map[string]string,
	body []byte,
	input any,
	document func([]byte, any) error,
) error {
	query := r.URL.Query()
	for name, mb := range b.Input {
		location, ok := strings.CutPrefix(mb.Location, unsupported)
		if !ok {
			continue
		}

		set := false
		switch location {
		case locationHeader:
			set = r.Header.Get(mb.Name) != ""
		case locationQuery:
			_, set = query[mb.Name]
		case locationPrefix:
			set = len(prefixed(r.Header, mb.Name)) > 0
		}

		if set {
			return NotImplemented(fmt.Sprintf("TinyStack does not support %s (%s %s).", name, location, mb.Name))
		}
	}

	var err error
	hasDocument := false
	fields(reflect.ValueOf(input).Elem(), b.Input, func(f reflect.Value, mb catalog.MemberBinding) {
		if err != nil {
			return
		}

		switch mb.Location {
		case locationLabel:
			err = setScalar(f, labels[mb.Name], mb.Name)
		case locationHeader:
			if v := r.Header.Get(mb.Name); v != "" {
				err = setScalar(f, v, mb.Name)
			}
		case locationQuery:
			if values, ok := query[mb.Name]; ok {
				// A list member repeats its key once per element.
				if f.Kind() == reflect.Slice && f.Type().Elem().Kind() == reflect.String {
					f.Set(reflect.ValueOf(values))
					return
				}

				if len(values) != 1 {
					err = invalid("Query parameter %s must be given once", mb.Name)
					return
				}

				err = setScalar(f, values[0], mb.Name)
			}
		case locationPrefix:
			if m := prefixed(r.Header, mb.Name); len(m) > 0 {
				f.Set(reflect.ValueOf(m))
			}
		case locationPayload:
			err = setPayload(f, body, document)
		case locationDocument:
			hasDocument = true
		}
	})
	if err != nil {
		return err
	}

	if hasDocument && len(bytes.TrimSpace(body)) > 0 {
		return document(body, input)
	}

	return nil
}

// prefixed collects headers starting with prefix, keyed by the lowercased rest.
func prefixed(header http.Header, prefix string) map[string]string {
	out := map[string]string{}
	for name, values := range header {
		lower := strings.ToLower(name)
		if rest, ok := strings.CutPrefix(lower, strings.ToLower(prefix)); ok && rest != "" {
			out[rest] = strings.Join(values, ",")
		}
	}

	return out
}

func setScalar(f reflect.Value, s, name string) error {
	if f.Kind() != reflect.Pointer {
		return fmt.Errorf("rest: %s is not a pointer", name)
	}

	v := reflect.New(f.Type().Elem())
	switch elem := v.Elem(); {
	case elem.Type() == timeType:
		t, err := http.ParseTime(s)
		if err != nil {
			return invalid("%s must be an HTTP date", name)
		}

		elem.Set(reflect.ValueOf(t))
	case elem.Kind() == reflect.String:
		elem.SetString(s)
	case elem.Kind() == reflect.Int64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return invalid("%s must be an integer", name)
		}

		elem.SetInt(n)
	case elem.Kind() == reflect.Bool:
		if s != "true" && s != "false" {
			return invalid("%s must be true or false", name)
		}

		elem.SetBool(s == "true")
	default:
		return fmt.Errorf("rest: unsupported kind %s for %s", elem.Kind(), name)
	}

	f.Set(v)
	return nil
}

// setPayload sets a payload member: a blob or string takes the raw body, and a
// structure is decoded from it.
func setPayload(f reflect.Value, body []byte, document func([]byte, any) error) error {
	if f.Kind() == reflect.Slice {
		f.SetBytes(body)
		return nil
	}

	if len(body) > 0 && f.Type().Elem().Kind() == reflect.String {
		f.Set(reflect.ValueOf(new(string(body))))
		return nil
	}

	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}

	v := reflect.New(f.Type().Elem())
	if err := document(body, v.Interface()); err != nil {
		return err
	}

	f.Set(v)
	return nil
}

func formatScalar(v reflect.Value) string {
	if v.Type() == timeType {
		t, _ := v.Interface().(time.Time)
		return t.UTC().Format(http.TimeFormat)
	}

	switch v.Kind() {
	case reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	default:
		return v.String()
	}
}

// Response is a success response with its HTTP-bound members applied.
type Response struct {
	// Structure is a structure payload member, if set, for the codec to
	// encode as the whole body; StructureName is its member name.
	Structure     any
	StructureName string
	// Payload is the raw payload member, if any.
	Payload []byte
	// Status is the binding's status code unless an httpResponseCode member
	// overrides it.
	Status int
	// Document reports whether the output has body members the codec encodes.
	Document bool
}

// Encode writes the header members of output and returns the rest of the
// response for the codec to finish.
func Encode(w http.ResponseWriter, b *catalog.Binding, output any) Response {
	resp := Response{Status: b.StatusCode}
	fields(reflect.ValueOf(output).Elem(), b.Output, func(f reflect.Value, mb catalog.MemberBinding) {
		switch mb.Location {
		case locationHeader:
			if !f.IsNil() {
				w.Header().Set(mb.Name, formatScalar(f.Elem()))
			}
		case locationPrefix:
			for k, val := range f.Interface().(map[string]string) {
				w.Header().Set(mb.Name+k, val)
			}
		case locationPayload:
			switch {
			case f.Kind() == reflect.Slice:
				resp.Payload = f.Bytes()
			case f.Kind() == reflect.Pointer && !f.IsNil() && f.Elem().Kind() == reflect.String:
				resp.Payload = []byte(f.Elem().String())
			case f.Kind() == reflect.Pointer && !f.IsNil():
				resp.Structure, resp.StructureName = f.Interface(), mb.Name
			}
		case locationStatus:
			if !f.IsNil() && f.Elem().Int() != 0 {
				resp.Status = int(f.Elem().Int())
			}
		case locationDocument:
			resp.Document = true
		}
	})
	// A single-part GET response with Content-Range is partial content. Smithy
	// S3 models expose this header but have no httpResponseCode output member.
	if b.Method == http.MethodGet && resp.Status == http.StatusOK && w.Header().Get("Content-Range") != "" {
		resp.Status = http.StatusPartialContent
	}

	return resp
}

// Send writes the status and body. HEAD responses carry headers only.
func Send(w http.ResponseWriter, status int, body []byte, head bool) {
	if w.Header().Get("Content-Length") == "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}

	w.WriteHeader(status)
	if !head {
		_, _ = w.Write(body)
	}
}
