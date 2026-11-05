package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/recent"
)

func TestRequestIdentifierKeepsBodiesRedacted(t *testing.T) {
	for _, tc := range []struct{ service, operation, member, target string }{
		{"lambda", "Invoke", "FunctionName", "tutorial-function"},
		{"secretsmanager", "GetSecretValue", "SecretId", "tutorial-secret"},
		{"ssm", "GetParameter", "Name", "tutorial-parameter:1"},
	} {
		for _, protocol := range []string{"console", catalog.AWSJSON11} {
			t.Run(tc.service+"/"+protocol, func(t *testing.T) {
				input := map[string]any{
					tc.member:      tc.target,
					"Value":        "parameter-value",
					"SecretString": "secret-value",
					"Payload":      "function-payload",
					"Input":        "workflow-input",
				}
				var envelope any = input
				path := "/"
				if protocol == "console" {
					envelope = map[string]any{"input": input}
					path = "/_tinystack/actions"
				}
				if tc.service == "lambda" && protocol != "console" {
					path = "/2015-03-31/functions/" + tc.target + "/invocations"
				}
				raw, err := json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest("POST", path, strings.NewReader(string(raw)))
				c := &call{
					meta: request.Metadata{Service: tc.service, Operation: tc.operation, Protocol: protocol},
					service: &catalog.Service{
						Operations: map[string]catalog.Operation{tc.operation: {Sensitive: true}},
					},
				}
				body := &capturingBody{}
				body.keep(raw)
				w := &capturingWriter{ResponseWriter: httptest.NewRecorder()}
				w.keep(
					[]byte(
						`{"SecretString":"secret-value","Value":"parameter-value",` +
							`"Payload":"function-payload","Output":"workflow-output"}`,
					),
				)
				s := &Server{
					requests: recent.New[Record](maxRecords, "requests"),
					metrics:  &metrics{},
					logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
				}
				s.record(r, c, time.Now(), body, w)
				rec := s.requests.List()[0]
				if rec.ResourceIdentifier != tc.target || rec.RequestBody != redacted || rec.ResponseBody != redacted {
					t.Fatalf("unexpected record: %+v", rec)
				}
				encoded, err := json.Marshal(rec)
				if err != nil {
					t.Fatal(err)
				}
				privateValues := []string{
					"secret-value", "parameter-value", "function-payload", "workflow-input", "workflow-output",
				}
				for _, secret := range privateValues {
					if strings.Contains(string(encoded), secret) {
						t.Fatalf("record exposed %s", secret)
					}
				}
			})
		}
	}
}

func TestRequestIdentifierFailsClosed(t *testing.T) {
	r := httptest.NewRequest("POST", "/", http.NoBody)
	c := &call{meta: request.Metadata{Service: "ssm", Operation: "GetParameter", Protocol: "console"}}
	bodies := []string{
		`{"input":{"Value":"private"}}`,
		`{"input":{"Name":{"Value":"private"}}}`,
		`{"Name":"outer","input":{}}`,
		`{"input":{"Name":"truncated"}`,
	}
	for _, body := range bodies {
		if got := requestResourceIdentifier(r, c, body); got != "" {
			t.Fatalf("unexpected identifier %q", got)
		}
	}
	c.meta.Operation = "PutParameter"
	if got := requestResourceIdentifier(r, c, `{"input":{"Name":"private","Value":"private"}}`); got != "" {
		t.Fatal(got)
	}
	c.meta.Operation = "GetParameter"
	c.sensitive = true
	if got := requestResourceIdentifier(r, c, `{"input":{"Name":"private"}}`); got != "" {
		t.Fatal(got)
	}
}
