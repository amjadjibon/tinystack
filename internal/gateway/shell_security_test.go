//go:build console

package gateway

import (
	"net/http/httptest"
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
