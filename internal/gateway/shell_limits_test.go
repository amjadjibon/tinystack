//go:build console

package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestShellCapacityAndBodyLimits(t *testing.T) {
	fakeShellCLI(t, "printf ran\n")
	s := shellServer(t)
	s.shellRunning.Store(shellMaxConcurrent)
	w := httptest.NewRecorder()
	s.serveShell(w, shellRequestFor("aws sts get-caller-identity"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatal(w.Code, w.Body)
	}

	s.shellRunning.Store(0)
	w = httptest.NewRecorder()
	s.serveShell(w, shellRequestFor("aws sts get-caller-identity"))
	if w.Code != http.StatusOK || s.shellRunning.Load() != 0 {
		t.Fatal("slot was not released", w.Code)
	}

	w = httptest.NewRecorder()
	s.serveShell(w, shellRequestFor("aws sts get-caller-identity "+strings.Repeat("x", shellMaxBody)))
	if w.Code != http.StatusBadRequest {
		t.Fatal("oversized command accepted", w.Code)
	}
}
