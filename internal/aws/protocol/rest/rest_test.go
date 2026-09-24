package rest

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

func TestDecodeQueryList(t *testing.T) {
	var in struct {
		One  *string  `json:"-"`
		Keys []string `json:"-"`
	}

	b := &catalog.Binding{Input: map[string]catalog.MemberBinding{
		"Keys": {Location: locationQuery, Name: "tagKeys"},
		"One":  {Location: locationQuery, Name: "one"},
	}}

	r := httptest.NewRequest(http.MethodDelete, "/t?tagKeys=a&tagKeys=b%20c&one=x", http.NoBody)
	if err := Decode(r, b, nil, nil, &in, nil); err != nil || strings.Join(in.Keys, ",") != "a,b c" ||
		*in.One != "x" {
		t.Fatalf("%v %+v", err, in)
	}

	r = httptest.NewRequest(http.MethodDelete, "/t?one=x&one=y", http.NoBody)
	if err := Decode(r, b, nil, nil, &in, nil); err == nil {
		t.Fatal("repeated scalar query parameter was accepted")
	}
}

func TestReadBodyDecodesAWSChunked(t *testing.T) {
	chunked := "5;chunk-signature=abc\r\nhello\r\n6\r\n world\r\n0\r\nx-amz-checksum-crc32:DUoRhQ==\r\n\r\n"
	r := httptest.NewRequest(http.MethodPut, "/b/k", strings.NewReader(chunked))
	r.Header.Set("Content-Encoding", "aws-chunked,gzip")
	body, err := ReadBody(httptest.NewRecorder(), r, 1<<20)
	if err != nil || string(body) != "hello world" {
		t.Fatalf("%q %v", body, err)
	}

	if r.Header.Get("Content-Encoding") != "gzip" || r.Header.Get("x-amz-checksum-crc32") != "DUoRhQ==" {
		t.Fatal(r.Header)
	}

	r = httptest.NewRequest(http.MethodPut, "/b/k", strings.NewReader("zz\r\nbad"))
	r.Header.Set("Content-Encoding", "aws-chunked")
	if _, err = ReadBody(httptest.NewRecorder(), r, 1<<20); err == nil {
		t.Fatal("malformed chunk accepted")
	}

	// Both plain and aws-chunked bodies are bounded by the limit.
	for _, body := range []string{"too large", "9\r\ntoo large\r\n0\r\n\r\n"} {
		r = httptest.NewRequest(http.MethodPut, "/b/k", strings.NewReader(body))
		if strings.Contains(body, "\r\n") {
			r.Header.Set("Content-Encoding", "aws-chunked")
		}

		_, err = ReadBody(httptest.NewRecorder(), r, 3)
		if e, ok := errors.AsType[*awserror.Error](err); !ok || e.Code != "EntityTooLarge" {
			t.Fatalf("oversized body %q: %v", body, err)
		}
	}
}
