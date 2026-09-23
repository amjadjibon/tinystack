package restxml

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

type input struct {
	Bucket   *string
	Key      *string
	Limit    *int64
	Since    *time.Time
	Metadata map[string]string
	Body     []byte
}

var binding = catalog.Binding{Input: map[string]catalog.MemberBinding{
	"Bucket":   {Location: "label", Name: "Bucket"},
	"Key":      {Location: "label", Name: "Key"},
	"Limit":    {Location: "query", Name: "max-keys"},
	"Since":    {Location: "header", Name: "If-Modified-Since"},
	"Metadata": {Location: "prefix", Name: "x-amz-meta-"},
	"Body":     {Location: "payload", Name: "Body"},
	"Range":    {Location: "unsupported:header", Name: "Range"},
}}

func TestDecodeBindsMembers(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/b/k?max-keys=5", strings.NewReader("data"))
	r.Header.Set("If-Modified-Since", "Mon, 02 Jan 2006 15:04:05 GMT")
	r.Header.Set("X-Amz-Meta-Owner", "ada")
	body, err := ReadBody(httptest.NewRecorder(), r, 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	var in input
	if err = Decode(r, &binding, map[string]string{"Bucket": "b", "Key": "a/b"}, body, &in); err != nil {
		t.Fatal(err)
	}

	if *in.Bucket != "b" || *in.Key != "a/b" || *in.Limit != 5 || in.Since.Year() != 2006 ||
		in.Metadata["owner"] != "ada" || string(in.Body) != "data" {
		t.Fatalf("%+v", in)
	}

	r.Header.Set("Range", "bytes=0-1")
	var e *awserror.Error
	if err = Decode(r, &binding, nil, body, &input{}); !errors.As(err, &e) || e.StatusCode != 501 {
		t.Fatalf("unsupported header accepted: %v", err)
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

	r = httptest.NewRequest(http.MethodPut, "/b/k", strings.NewReader("too large"))
	var e *awserror.Error
	if _, err = ReadBody(httptest.NewRecorder(), r, 3); !errors.As(err, &e) || e.Code != "EntityTooLarge" {
		t.Fatalf("oversized body: %v", err)
	}
}

func TestDecodeXMLRejectsUnknownElements(t *testing.T) {
	var cfg struct {
		LocationConstraint *string `xml:"LocationConstraint,omitempty"`
	}

	if err := decodeXML([]byte(`<C><LocationConstraint>eu-west-1</LocationConstraint></C>`), &cfg); err != nil ||
		*cfg.LocationConstraint != "eu-west-1" {
		t.Fatal(err)
	}

	if err := decodeXML([]byte(`<C><Tags/></C>`), &cfg); err == nil {
		t.Fatal("unknown element accepted")
	}

	if err := decodeXML([]byte(`<C>`), &cfg); err == nil {
		t.Fatal("malformed XML accepted")
	}
}
