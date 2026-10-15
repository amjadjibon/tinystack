//go:build console

package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestShellInspectorRedactsSecrets(t *testing.T) {
	fakeShellCLI(t, "printf secret-response\n")
	s := shellServer(t)
	w := httptest.NewRecorder()
	s.serveShell(w, shellRequestFor("aws sqs send-message --message-body secret-request"))
	records := s.requests.List()
	if len(records) != 1 {
		t.Fatalf("expected one inspector record, got %d", len(records))
	}

	for _, rec := range records {
		if rec.RequestBody != redacted || rec.ResponseBody != redacted {
			t.Fatalf("CloudShell retained secrets: %+v", rec)
		}
	}
}

func TestShellInspectorRedactsValidationErrors(t *testing.T) {
	s := shellServer(t)
	w := httptest.NewRecorder()
	r := shellRequestFor("aws sts get-caller-identity")
	r.Body = io.NopCloser(strings.NewReader(`{"secret-error":true}`))
	s.serveShell(w, r)
	records := s.requests.List()
	if w.Code != http.StatusBadRequest || len(records) != 1 {
		t.Fatal(w.Code, records)
	}

	rec := records[0]
	if rec.RequestBody != redacted || rec.ResponseBody != redacted || rec.ErrorMessage != redacted {
		t.Fatalf("CloudShell retained rejected input: %+v", rec)
	}
}
