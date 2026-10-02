package console

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestHandler(t *testing.T) {
	files := fstest.MapFS{
		"_shell.html":         {Data: []byte("<html>shell</html>")},
		"assets/index-abc.js": {Data: []byte("console.log(1)")},
		"aws-icons/sqs.svg":   {Data: []byte("<svg/>")},
	}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "api "+r.URL.Path)
	})
	h, err := Handler(files, api)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		method, path, body, cache string
		status                    int
	}{
		{http.MethodGet, "/", "<html>shell</html>", "no-cache", http.StatusOK},
		{http.MethodGet, "/services/sqs", "<html>shell</html>", "no-cache", http.StatusOK},
		{http.MethodGet, "/assets/index-abc.js", "console.log(1)", "public, max-age=31536000, immutable", http.StatusOK},
		{http.MethodGet, "/aws-icons/sqs.svg", "<svg/>", "", http.StatusOK},
		{http.MethodGet, "/_tinystack/health", "api /_tinystack/health", "", http.StatusOK},
		{http.MethodDelete, "/_tinystack/requests", "api /_tinystack/requests", "", http.StatusOK},
		{http.MethodPost, "/", "", "", http.StatusMethodNotAllowed},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, http.NoBody))
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.body) ||
			rec.Header().Get("Cache-Control") != tc.cache {
			t.Errorf("%s %s: %d %q %v", tc.method, tc.path, rec.Code, rec.Body, rec.Header())
		}
	}
}

func TestHandlerNeedsShell(t *testing.T) {
	if _, err := Handler(fstest.MapFS{}, http.NotFoundHandler()); err == nil {
		t.Fatal("built a console without its shell page")
	}
}
