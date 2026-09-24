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
	"github.com/amjadjibon/tinystack/internal/aws/protocol/rest"
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
	body, err := rest.ReadBody(httptest.NewRecorder(), r, 1<<20)
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
