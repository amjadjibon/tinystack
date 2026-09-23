package query

import (
	"errors"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

type inner struct {
	Label *string `query:"Label"`
	Blob  []byte  `query:"Blob"`
}

type fixture struct {
	Name    *string           `query:"Name"`
	Count   *int64            `query:"Count"`
	Enabled *bool             `query:"Enabled"`
	Ratio   *float64          `query:"Ratio"`
	Nested  *inner            `query:"Nested"`
	Ids     []string          `query:"Ids,member=Id"`
	Flat    []inner           `query:"Item,flat"`
	Tags    map[string]string `query:"Tags"`
	Attrs   map[string]inner  `query:"Attr,flat,key=Name,value=Value"`
	Empty   []string          `query:"Empty"`
}

func TestDecode(t *testing.T) {
	params, err := url.ParseQuery(
		"Action=Op&Version=1&Name=a%20b&Count=-3&Enabled=true&Ratio=1.5&Nested.Label=n&Nested.Blob=AAE%3D" +
			"&Ids.Id.1=x&Ids.Id.2=y&Item.1.Label=i1&Item.2.Label=i2" +
			"&Tags.entry.1.key=k&Tags.entry.1.value=v" +
			"&Attr.1.Name=color&Attr.1.Value.Label=blue&Empty=",
	)
	if err != nil {
		t.Fatal(err)
	}

	var got fixture
	if err = Decode(params, &got); err != nil {
		t.Fatal(err)
	}

	label := func(s string) *string { return &s }
	count, enabled, ratio := int64(-3), true, 1.5
	want := fixture{
		Name: label("a b"), Count: &count, Enabled: &enabled, Ratio: &ratio,
		Nested: &inner{Label: label("n"), Blob: []byte{0, 1}},
		Ids:    []string{"x", "y"},
		Flat:   []inner{{Label: label("i1")}, {Label: label("i2")}},
		Tags:   map[string]string{"k": "v"},
		Attrs:  map[string]inner{"color": {Label: label("blue")}},
		Empty:  []string{},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestDecodeRejects(t *testing.T) {
	for name, query := range map[string]string{
		"unknown":        "Bogus=1",
		"unknown nested": "Nested.Other=1",
		"gap":            "Ids.Id.1=x&Ids.Id.3=z",
		"flat as nested": "Ids.1=x",
		"duplicate":      "Name=a&Name=b",
		"bad integer":    "Count=1.5",
		"bad bool":       "Enabled=yes",
		"bad blob":       "Nested.Blob=!!",
		"missing key":    "Tags.entry.1.value=v",
		"missing value":  "Tags.entry.1.key=k",
		"duplicate key":  "Tags.entry.1.key=k&Tags.entry.1.value=v&Tags.entry.2.key=k&Tags.entry.2.value=w",
	} {
		t.Run(name, func(t *testing.T) {
			params, err := url.ParseQuery(query)
			if err != nil {
				t.Fatal(err)
			}

			var e *awserror.Error
			if err = Decode(params, &fixture{}); !errors.As(err, &e) || e.Code != "InvalidParameterValue" {
				t.Fatalf("accepted or wrong error: %v", err)
			}
		})
	}
}

func TestWrite(t *testing.T) {
	label := "a<b"
	w := httptest.NewRecorder()
	Write(w, 200, "Op", "urn:ns", "req-1", &fixture{
		Name:  &label,
		Ids:   []string{"x"},
		Flat:  []inner{{Label: &label}},
		Tags:  map[string]string{"b": "2", "a": "1"},
		Attrs: map[string]inner{"k": {Blob: []byte{0, 1}}},
	})
	want := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<OpResponse xmlns="urn:ns"><OpResult><Name>a&lt;b</Name><Ids><Id>x</Id></Ids>` +
		`<Item><Label>a&lt;b</Label></Item>` +
		`<Tags><entry><key>a</key><value>1</value></entry><entry><key>b</key><value>2</value></entry></Tags>` +
		`<Attr><Name>k</Name><Value><Blob>AAE=</Blob></Value></Attr></OpResult>` +
		`<ResponseMetadata><RequestId>req-1</RequestId></ResponseMetadata></OpResponse>`
	if got := w.Body.String(); got != want || w.Header().Get("Content-Type") != "text/xml" {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}

	w = httptest.NewRecorder()
	Write(w, 200, "Unit", "urn:ns", "req-2", &struct{}{})
	if strings.Contains(w.Body.String(), "UnitResult") {
		t.Fatal("empty output wrote a result element")
	}

	w = httptest.NewRecorder()
	WriteError(w, "urn:ns", "req-3", &awserror.Error{
		Code:       "QueueDoesNotExist",
		QueryCode:  "AWS.SimpleQueueService.NonExistentQueue",
		Message:    "gone",
		StatusCode: 400,
	})
	if !strings.Contains(w.Body.String(), "<Type>Sender</Type><Code>AWS.SimpleQueueService.NonExistentQueue</Code>") ||
		w.Code != 400 {
		t.Fatal(w.Body.String())
	}
}
