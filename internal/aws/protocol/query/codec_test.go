package query

import (
	"errors"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

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
	At      *time.Time        `query:"At"`
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
			"&Attr.1.Name=color&Attr.1.Value.Label=blue&Empty=&At=2024-05-06T07%3A08%3A09Z",
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
	at := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	want := fixture{
		Name: label("a b"), Count: &count, Enabled: &enabled, Ratio: &ratio,
		Nested: &inner{Label: label("n"), Blob: []byte{0, 1}},
		Ids:    []string{"x", "y"},
		Flat:   []inner{{Label: label("i1")}, {Label: label("i2")}},
		Tags:   map[string]string{"k": "v"},
		Attrs:  map[string]inner{"color": {Label: label("blue")}},
		Empty:  []string{},
		At:     &at,
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
		"bad timestamp":  "At=yesterday",
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
	at := time.Date(2024, 5, 6, 7, 8, 9, 0, time.FixedZone("", 3600))
	w := httptest.NewRecorder()
	Write(w, 200, "Op", "urn:ns", "req-1", &fixture{
		Name:  &label,
		Ids:   []string{"x"},
		Flat:  []inner{{Label: &label}},
		Tags:  map[string]string{"b": "2", "a": "1"},
		Attrs: map[string]inner{"k": {Blob: []byte{0, 1}}},
		At:    &at,
	})
	want := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<OpResponse xmlns="urn:ns"><OpResult><Name>a&lt;b</Name><At>2024-05-06T06:08:09Z</At><Ids><Id>x</Id></Ids>` +
		`<Item><Label>a&lt;b</Label></Item>` +
		`<Tags><entry><key>a</key><value>1</value></entry><entry><key>b</key><value>2</value></entry></Tags>` +
		`<Attr><Name>k</Name><Value><Blob>AAE=</Blob></Value></Attr></OpResult>` +
		`<ResponseMetadata><RequestId>req-1</RequestId></ResponseMetadata></OpResponse>`
	if got := w.Body.String(); got != want || w.Header().Get("Content-Type") != "text/xml" {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}

	w = httptest.NewRecorder()
	Write(w, 200, "Unit", "urn:ns", "req-2", nil)
	if strings.Contains(w.Body.String(), "UnitResult") {
		t.Fatal("unit output wrote a result element")
	}

	w = httptest.NewRecorder()
	Write(w, 200, "Empty", "urn:ns", "req-2", &struct{}{})
	if !strings.Contains(w.Body.String(), "<EmptyResult></EmptyResult>") {
		t.Fatalf("empty structure output wrote no result element: %s", w.Body.String())
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

type ec2Filter struct {
	Name   *string  `ec2:"Name,name"`
	Values []string `ec2:"Value,value,member=item"`
}

type ec2Fixture struct {
	DryRun  *bool       `ec2:"DryRun,dryRun"`
	IDs     []string    `ec2:"InstanceId,instanceId,member=item"`
	Filters []ec2Filter `ec2:"Filter,filter,member=item"`
}

// TestEC2 decodes flat EC2 Query lists, including lists in list members, and
// writes the response and error envelopes.
func TestEC2(t *testing.T) {
	params, err := url.ParseQuery("Action=Op&Version=1&DryRun=true&InstanceId.1=i-1&InstanceId.2=i-2" +
		"&Filter.1.Name=tag%3Ateam&Filter.1.Value.1=a&Filter.1.Value.2=b")
	if err != nil {
		t.Fatal(err)
	}

	var got ec2Fixture
	if err = DecodeEC2(params, &got); err != nil {
		t.Fatal(err)
	}

	if !*got.DryRun || !reflect.DeepEqual(got.IDs, []string{"i-1", "i-2"}) || len(got.Filters) != 1 ||
		*got.Filters[0].Name != "tag:team" || !reflect.DeepEqual(got.Filters[0].Values, []string{"a", "b"}) {
		t.Fatalf("got %+v", got)
	}

	// AWS Query's wrapped list keys are not EC2 Query keys.
	params.Set("InstanceId.member.1", "i-3")
	if err = DecodeEC2(params, &got); err == nil {
		t.Fatal("decoded an AWS Query list key")
	}

	w := httptest.NewRecorder()
	WriteEC2(w, 200, "Op", "urn:ec2", "req-1", &got)
	want := `<OpResponse xmlns="urn:ec2"><requestId>req-1</requestId><dryRun>true</dryRun>` +
		`<instanceId><item>i-1</item><item>i-2</item></instanceId><filter><item><name>tag:team</name>` +
		`<value><item>a</item><item>b</item></value></item></filter></OpResponse>`
	if body := w.Body.String(); !strings.HasSuffix(body, want) {
		t.Fatalf("got %s", body)
	}

	w = httptest.NewRecorder()
	WriteEC2Error(w, "req-2", awserror.New("InvalidGroup.NotFound", "no <group>"))
	want = `<Response><Errors><Error><Code>InvalidGroup.NotFound</Code><Message>no &lt;group&gt;</Message>` +
		`</Error></Errors><RequestID>req-2</RequestID></Response>`
	if body := w.Body.String(); w.Code != 400 || !strings.HasSuffix(body, want) {
		t.Fatalf("got %d %s", w.Code, body)
	}
}
