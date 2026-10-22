// Package rest implements the HTTP bindings shared by the AWS REST protocols:
// catalog.MemberBinding places each member in the URI, headers, query string,
// payload, or status code. The restxml and restjson codecs supply the document
// format.
package rest

import (
	"bufio"
	"bytes"
	"context"
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

type chunkState int

const (
	stateChunkHeader chunkState = iota
	stateChunkData
	stateChunkCRLF
	stateTrailers
	stateEOF
)

const (
	maxFramingLine       = 4096
	maxDelimiterLine     = 64
	maxAggregateTrailers = 64 << 10 // 64 KiB
	maxTrailerCount      = 128
)

var errFramingTooLarge = errors.New("framing line too long")

func readBoundedLine(r *bufio.Reader, maxLen int) (string, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return "", err
		}
		if len(buf)+len(chunk) > maxLen {
			return "", errFramingTooLarge
		}
		buf = append(buf, chunk...)
		if !isPrefix {
			break
		}
	}
	return string(buf), nil
}

type chunkReader struct {
	r                 *bufio.Reader
	header            http.Header
	remaining         int64
	totalTrailerBytes int
	trailerCount      int
	state             chunkState
}

func (c *chunkReader) malformed(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return TooLarge()
	}

	return invalid("Malformed aws-chunked request body")
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	for {
		switch c.state {
		case stateChunkHeader:
			line, err := readBoundedLine(c.r, maxFramingLine)
			if err != nil {
				return 0, c.malformed(err)
			}

			sizeText, _, _ := strings.Cut(line, ";")
			size, err := strconv.ParseInt(strings.TrimSpace(sizeText), 16, 64)
			if err != nil || size < 0 {
				return 0, c.malformed(nil)
			}

			if size == 0 {
				c.state = stateTrailers
				continue
			}

			c.remaining = size
			c.state = stateChunkData

		case stateChunkData:
			toRead := len(p)
			if int64(toRead) > c.remaining {
				toRead = int(c.remaining)
			}

			n, err := c.r.Read(p[:toRead])
			c.remaining -= int64(n)
			if c.remaining == 0 {
				c.state = stateChunkCRLF
			}

			if err != nil && !errors.Is(err, io.EOF) {
				return n, c.malformed(err)
			}

			if n > 0 {
				return n, nil
			}

			if errors.Is(err, io.EOF) && c.remaining > 0 {
				return 0, c.malformed(err)
			}

		case stateChunkCRLF:
			crlf, err := readBoundedLine(c.r, maxDelimiterLine)
			if err != nil || crlf != "" {
				return 0, c.malformed(err)
			}

			c.state = stateChunkHeader

		case stateTrailers:
			for {
				line, err := readBoundedLine(c.r, maxFramingLine)
				if err != nil {
					return 0, c.malformed(err)
				}

				if line == "" {
					break
				}

				c.totalTrailerBytes += len(line)
				c.trailerCount++
				if c.totalTrailerBytes > maxAggregateTrailers || c.trailerCount > maxTrailerCount {
					return 0, c.malformed(errFramingTooLarge)
				}

				name, value, ok := strings.Cut(line, ":")
				if !ok {
					return 0, c.malformed(nil)
				}

				if c.header != nil {
					c.header.Set(strings.TrimSpace(name), strings.TrimSpace(value))
				}
			}

			extra, err := c.r.Peek(1)
			if len(extra) > 0 {
				return 0, c.malformed(nil)
			}
			if !errors.Is(err, io.EOF) {
				return 0, c.malformed(err)
			}

			c.state = stateEOF
			return 0, io.EOF

		case stateEOF:
			return 0, io.EOF
		}
	}
}

// NewChunkReader decodes "size[;chunk-signature=...]\r\ndata\r\n" chunks up to the
// zero-length chunk, then reads "name:value" trailers into header.
func NewChunkReader(r *bufio.Reader, header http.Header) io.Reader {
	return &chunkReader{r: r, header: header}
}

// dechunk decodes "size[;chunk-signature=...]\\r\\ndata\\r\\n" chunks up to the
// zero-length chunk, then reads "name:value" trailers into header.
func dechunk(r *bufio.Reader, header http.Header) ([]byte, error) {
	return io.ReadAll(NewChunkReader(r, header))
}

// StreamLimiter bounds decoded bytes read from an upload and validates expected lengths.
type StreamLimiter struct {
	r           io.Reader
	closer      io.Closer
	trailers    http.Header
	req         *http.Request
	maxBytes    int64
	expectedLen int64
	readBytes   int64
}

func incompleteBody() error {
	return awserror.New(
		"IncompleteBody",
		"You did not provide the number of bytes specified by the Content-Length HTTP header.",
	)
}

func (s *StreamLimiter) Read(p []byte) (int, error) {
	if s.req != nil && s.req.Context().Err() != nil {
		return 0, s.req.Context().Err()
	}

	n, err := s.r.Read(p)
	s.readBytes += int64(n)
	if s.readBytes > s.maxBytes {
		return n, TooLarge()
	}

	if s.expectedLen >= 0 && s.readBytes > s.expectedLen {
		return n, incompleteBody()
	}

	if err != nil {
		if s.req != nil && s.req.Context().Err() != nil {
			return n, s.req.Context().Err()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return n, err
		}

		if errors.Is(err, io.EOF) {
			if s.expectedLen >= 0 && s.readBytes != s.expectedLen {
				return n, incompleteBody()
			}

			if s.req != nil {
				for k, vals := range s.req.Trailer {
					for _, v := range vals {
						s.trailers.Add(k, v)
					}
					lower := strings.ToLower(k)
					if (strings.HasPrefix(lower, "x-amz-checksum-") || lower == "content-md5") &&
						s.trailers.Get(k) == "" {
						return n, invalid("Missing advertised trailer: %s", k)
					}
				}

				for _, headerName := range []string{"x-amz-trailer", "Trailer"} {
					for _, advertised := range s.req.Header.Values(headerName) {
						for _, name := range strings.Split(advertised, ",") {
							name = strings.TrimSpace(name)
							if name == "" {
								continue
							}
							lower := strings.ToLower(name)
							match := headerName == "x-amz-trailer" ||
								strings.HasPrefix(lower, "x-amz-checksum-") ||
								lower == "content-md5"
							if match && s.trailers.Get(name) == "" {
								return n, invalid("Missing advertised trailer: %s", name)
							}
						}
					}
				}
			}
		} else if errors.Is(err, io.ErrUnexpectedEOF) {
			return n, incompleteBody()
		} else if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return n, TooLarge()
		}
	}

	return n, err
}

func (s *StreamLimiter) Close() error {
	if s.closer != nil {
		return s.closer.Close()
	}

	return nil
}

func (s *StreamLimiter) Trailers() http.Header {
	return s.trailers
}

// StreamBody prepares a bounded streaming reader for an upload request, decoding
// aws-chunked streams on the fly and validating declared bounds.
func StreamBody(w http.ResponseWriter, r *http.Request, maxBytes int64) (io.ReadCloser, func() http.Header, error) {
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

	trailers := make(http.Header)
	var expectedLen int64 = -1
	var bodyReader io.Reader

	if chunked {
		if decodedLenStr := r.Header.Get("x-amz-decoded-content-length"); decodedLenStr != "" {
			l, err := strconv.ParseInt(strings.TrimSpace(decodedLenStr), 10, 64)
			if err != nil || l < 0 {
				return nil, nil, invalid("x-amz-decoded-content-length must be a non-negative integer")
			}
			expectedLen = l
			r.Header.Set("Content-Length", strconv.FormatInt(l, 10))
		} else {
			r.Header.Del("Content-Length")
		}

		if expectedLen > maxBytes {
			return nil, nil, TooLarge()
		}

		encodedLimit := maxBytes + max(8<<20, maxBytes/8)
		reader := http.MaxBytesReader(w, r.Body, encodedLimit)
		bodyReader = NewChunkReader(bufio.NewReader(reader), trailers)
	} else {
		if clStr := r.Header.Get("Content-Length"); clStr != "" {
			l, err := strconv.ParseInt(strings.TrimSpace(clStr), 10, 64)
			if err != nil || l < 0 {
				return nil, nil, invalid("Content-Length must be a non-negative integer")
			}
			expectedLen = l
		} else if r.ContentLength >= 0 {
			expectedLen = r.ContentLength
		}

		if expectedLen > maxBytes {
			return nil, nil, TooLarge()
		}

		reader := http.MaxBytesReader(w, r.Body, maxBytes)
		bodyReader = reader
	}

	limiter := &StreamLimiter{
		r:           bodyReader,
		closer:      r.Body,
		maxBytes:    maxBytes,
		expectedLen: expectedLen,
		trailers:    trailers,
		req:         r,
	}

	return limiter, limiter.Trailers, nil
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
			if t, err = time.Parse(time.RFC3339, s); err != nil {
				return invalid("%s must be an HTTP date or ISO 8601 timestamp", name)
			}
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

func formatHeader(mb catalog.MemberBinding, v reflect.Value) string {
	if v.Type() == timeType {
		t, _ := v.Interface().(time.Time)
		if mb.Format == "date-time" {
			return t.UTC().Format(time.RFC3339)
		}

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
				w.Header().Set(mb.Name, formatHeader(mb, f.Elem()))
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
