// Package restxml implements the AWS REST XML protocol from generated bindings:
// catalog.MemberBinding places each member in the HTTP message, and generated
// encoding/xml tags describe XML bodies.
package restxml

import (
	"bufio"
	"bytes"
	"encoding/xml"
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

	var tooLarge *http.MaxBytesError
	if !chunked {
		body, err := io.ReadAll(reader)
		if errors.As(err, &tooLarge) {
			return nil, &awserror.Error{
				Code:       "EntityTooLarge",
				Message:    "Your proposed upload exceeds the maximum allowed size",
				StatusCode: 400,
			}
		}

		return body, err
	}

	body, err := dechunk(bufio.NewReader(reader), r.Header)
	if errors.As(err, &tooLarge) {
		return nil, &awserror.Error{
			Code: "EntityTooLarge", Message: "Your proposed upload exceeds the maximum allowed size", StatusCode: 400,
		}
	}

	return body, err
}

// dechunk decodes "size[;chunk-signature=...]\r\ndata\r\n" chunks up to the
// zero-length chunk, then reads "name:value" trailers into header.
func dechunk(r *bufio.Reader, header http.Header) ([]byte, error) {
	malformed := invalid("Malformed aws-chunked request body")
	var body bytes.Buffer
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, malformed
		}

		sizeText, _, _ := strings.Cut(strings.TrimRight(line, "\r\n"), ";")
		size, err := strconv.ParseInt(strings.TrimSpace(sizeText), 16, 64)
		if err != nil || size < 0 {
			return nil, malformed
		}

		if size == 0 {
			break
		}

		if _, err = io.CopyN(&body, r, size); err != nil {
			return nil, malformed
		}

		if crlf, crlfErr := r.ReadString('\n'); crlfErr != nil || strings.TrimRight(crlf, "\r\n") != "" {
			return nil, malformed
		}
	}

	for {
		line, err := r.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, malformed
		}

		if line = strings.TrimRight(line, "\r\n"); line != "" {
			name, value, ok := strings.Cut(line, ":")
			if !ok {
				return nil, malformed
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
// body the request payload. Modeled inputs the service does not implement fail.
func Decode(r *http.Request, b *catalog.Binding, labels map[string]string, body []byte, input any) error {
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
	document := false
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
			err = setPayload(f, body)
		case locationDocument:
			document = true
		}
	})
	if err != nil {
		return err
	}

	if document && len(body) > 0 {
		return decodeXML(body, input)
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
		return fmt.Errorf("restxml: %s is not a pointer", name)
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
		return fmt.Errorf("restxml: unsupported kind %s for %s", elem.Kind(), name)
	}

	f.Set(v)
	return nil
}

func setPayload(f reflect.Value, body []byte) error {
	if f.Kind() == reflect.Slice {
		f.SetBytes(body)
		return nil
	}

	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}

	v := reflect.New(f.Type().Elem())
	if err := decodeXML(body, v.Interface()); err != nil {
		return err
	}

	f.Set(v)
	return nil
}

// decodeXML decodes a body strictly: elements the generated type does not
// declare fail instead of being ignored.
func decodeXML(body []byte, v any) error {
	malformed := awserror.New("MalformedXML", "The XML you provided was not well-formed or did not validate")
	if err := xml.Unmarshal(body, v); err != nil {
		return malformed
	}

	known := map[string]bool{}
	t := reflect.TypeOf(v).Elem()
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("xml"), ",")
		name, _, _ = strings.Cut(name, ">")
		if name != "" && name != "-" && t.Field(i).Name != "XMLName" {
			known[name] = true
		}
	}

	dec := xml.NewDecoder(bytes.NewReader(body))
	depth := 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return malformed
		}

		switch el := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 2 && !known[el.Name.Local] {
				return NotImplemented("TinyStack does not support the " + el.Name.Local + " element.")
			}
		case xml.EndElement:
			depth--
		}
	}
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

// Write encodes a successful response. HEAD responses carry headers only.
func Write(w http.ResponseWriter, b *catalog.Binding, output any, head bool) {
	var body []byte
	document := false
	v := reflect.ValueOf(output).Elem()
	fields(v, b.Output, func(f reflect.Value, mb catalog.MemberBinding) {
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
			if f.Kind() == reflect.Slice {
				body = f.Bytes()
			}
		case locationDocument:
			document = true
		}
	})

	if document {
		encoded, err := xml.Marshal(output)
		if err != nil {
			WriteError(w, awserror.From(err), "", head)
			return
		}

		body = append([]byte(xml.Header), encoded...)
		w.Header().Set("Content-Type", "application/xml")
	}

	if w.Header().Get("Content-Length") == "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}

	w.WriteHeader(b.StatusCode)
	if !head {
		_, _ = w.Write(body)
	}
}

// WriteError encodes the unwrapped S3-style REST XML error. HEAD responses
// carry no body, so clients infer the error from the status code.
func WriteError(w http.ResponseWriter, e *awserror.Error, requestID string, head bool) {
	var b bytes.Buffer
	b.WriteString(xml.Header + "<Error><Code>")
	_ = xml.EscapeText(&b, []byte(e.Code))
	b.WriteString("</Code><Message>")
	_ = xml.EscapeText(&b, []byte(e.Message))
	b.WriteString("</Message><RequestId>")
	_ = xml.EscapeText(&b, []byte(requestID))
	b.WriteString("</RequestId></Error>")
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Del("Content-Length")
	w.WriteHeader(e.StatusCode)
	if !head {
		_, _ = w.Write(b.Bytes())
	}
}
