package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	queryapi "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamquery"
	writeapi "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamwrite"
	"github.com/amjadjibon/tinystack/internal/config"
)

// Every operation in timestreamquery and timestreamwrite must reach its service despite sharing both the
// "timestream" signing name and the "Timestream_20181101" JSON target prefix, regardless of registration order.
func TestTimestreamOperationRouting(t *testing.T) {
	descriptors := []catalog.Service{
		queryapi.Descriptor(nil),
		writeapi.Descriptor(nil),
	}

	type testInput struct {
		ResourceARN *string        `json:"ResourceARN"`
		MaxResults  *int64         `json:"MaxResults"`
		Tags        []queryapi.Tag `json:"Tags"`
		TagKeys     []string       `json:"TagKeys"`
	}

	for i := range descriptors {
		service := &descriptors[i]
		for name, operation := range service.Operations {
			marker := service.Name + "." + name
			operation.NewInput = func() any { return &testInput{} }
			operation.Invoke = func(context.Context, any) (any, error) {
				return nil, awserror.New("ValidationException", marker)
			}

			service.Operations[name] = operation
		}
	}

	queryDesc := queryapi.Descriptor(nil)
	writeDesc := writeapi.Descriptor(nil)

	for _, reverse := range []bool{false, true} {
		if reverse {
			for i, j := 0, len(descriptors)-1; i < j; i, j = i+1, j-1 {
				descriptors[i], descriptors[j] = descriptors[j], descriptors[i]
			}
		}

		gw, err := New(config.Default(), slog.New(slog.NewTextHandler(io.Discard, nil)), descriptors...)
		if err != nil {
			t.Fatal(err)
		}

		call := func(t *testing.T, op, body, wantService string) {
			t.Helper()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/x-amz-json-1.0")
			req.Header.Set("X-Amz-Target", "Timestream_20181101."+op)
			req.Header.Set(
				"Authorization",
				"AWS4-HMAC-SHA256 Credential=test/20261025/us-east-1/timestream/aws4_request, "+
					"SignedHeaders=host;x-amz-date, Signature=unused",
			)
			req.Header.Set("X-Amz-Date", "20261025T000000Z")
			out := httptest.NewRecorder()
			gw.ServeHTTP(out, req)
			if !strings.Contains(out.Body.String(), wantService+"."+op) {
				t.Fatalf("wrong dispatch: %d %s (want %s.%s)", out.Code, out.Body.String(), wantService, op)
			}
		}

		for name := range queryDesc.Operations {
			if _, shared := writeDesc.Operations[name]; shared {
				continue
			}

			t.Run(fmt.Sprintf("%t/timestreamquery/%s", reverse, name), func(t *testing.T) {
				call(t, name, `{}`, "timestreamquery")
			})
		}

		for name := range writeDesc.Operations {
			if _, shared := queryDesc.Operations[name]; shared {
				continue
			}

			t.Run(fmt.Sprintf("%t/timestreamwrite/%s", reverse, name), func(t *testing.T) {
				call(t, name, `{}`, "timestreamwrite")
			})
		}

		for _, tc := range []struct {
			label   string
			op      string
			body    string
			service string
		}{
			{
				label:   "DescribeEndpoints",
				op:      "DescribeEndpoints",
				body:    `{}`,
				service: "timestreamwrite",
			},
			{
				label:   "TagResource/scheduled-query",
				op:      "TagResource",
				body:    `{"ResourceARN":"arn:aws:timestream:us-east-1:123456789012:scheduled-query/q1","Tags":[]}`,
				service: "timestreamquery",
			},
			{
				label:   "UntagResource/scheduled-query",
				op:      "UntagResource",
				body:    `{"ResourceARN":"arn:aws:timestream:us-east-1:123456789012:scheduled-query/q1","TagKeys":["k"]}`,
				service: "timestreamquery",
			},
			{
				label:   "ListTagsForResource/scheduled-query",
				op:      "ListTagsForResource",
				body:    `{"ResourceARN":"arn:aws:timestream:us-east-1:123456789012:scheduled-query/q1"}`,
				service: "timestreamquery",
			},
			{
				label:   "TagResource/table",
				op:      "TagResource",
				body:    `{"ResourceARN":"arn:aws:timestream:us-east-1:123456789012:database/db1/table/t1","Tags":[]}`,
				service: "timestreamwrite",
			},
			{
				label:   "UntagResource/table",
				op:      "UntagResource",
				body:    `{"ResourceARN":"arn:aws:timestream:us-east-1:123456789012:database/db1/table/t1","TagKeys":["k"]}`,
				service: "timestreamwrite",
			},
			{
				label: "ListTagsForResource/table-with-MaxResults",
				op:    "ListTagsForResource",
				body: `{"ResourceARN":"arn:aws:timestream:us-east-1:123456789012:database/db1/table/t1",` +
					`"MaxResults":10}`,
				service: "timestreamwrite",
			},
		} {
			t.Run(fmt.Sprintf("%t/shared/%s", reverse, tc.label), func(t *testing.T) {
				call(t, tc.op, tc.body, tc.service)
			})
		}
	}
}
