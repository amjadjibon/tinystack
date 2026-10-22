package console

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func gzipped(t *testing.T, data string) *fstest.MapFile {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := io.WriteString(zw, data); err != nil {
		t.Fatal(err)
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	return &fstest.MapFile{Data: buf.Bytes()}
}

func TestHandler(t *testing.T) {
	files := fstest.MapFS{
		"_shell.html.gz":         gzipped(t, "<html>shell</html>"),
		"assets/index-abc.js.gz": gzipped(t, "console.log(1)"),
		"aws-icons/sqs.svg.gz":   gzipped(t, "<svg/>"),
	}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "api "+r.URL.Path)
	})
	h, err := Handler(files, api, "127.0.0.1:4567")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		method, path, accept, body, cache, encoding, contentType string
		status                                                   int
	}{
		{http.MethodGet, "/", "", "<html>shell</html>", "no-cache", "", "text/html; charset=utf-8", http.StatusOK},
		{
			http.MethodGet, "/services/sqs", "gzip", "<html>shell</html>", "no-cache", "", "text/html; charset=utf-8",
			http.StatusOK,
		},
		{
			http.MethodGet, "/assets/index-abc.js", "gzip, br", "console.log(1)", "public, max-age=31536000, immutable",
			"gzip", "text/javascript; charset=utf-8", http.StatusOK,
		},
		{
			http.MethodGet, "/assets/index-abc.js", "", "console.log(1)", "public, max-age=31536000, immutable", "",
			"text/javascript; charset=utf-8", http.StatusOK,
		},
		{http.MethodGet, "/aws-icons/sqs.svg", "gzip;q=0", "<svg/>", "", "", "image/svg+xml", http.StatusOK},
		{http.MethodGet, "/aws-icons/sqs.svg", "*", "<svg/>", "", "gzip", "image/svg+xml", http.StatusOK},
		{http.MethodGet, "/_tinystack/health", "", "api /_tinystack/health", "", "", "", http.StatusOK},
		{http.MethodDelete, "/_tinystack/requests", "", "api /_tinystack/requests", "", "", "", http.StatusOK},
		{http.MethodPost, "/", "", "", "", "", "", http.StatusMethodNotAllowed},
	} {
		req := httptest.NewRequest(tc.method, tc.path, http.NoBody)
		if tc.accept != "" {
			req.Header.Set("Accept-Encoding", tc.accept)
		}

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		if tc.encoding == "gzip" {
			zr, zErr := gzip.NewReader(rec.Body)
			if zErr != nil {
				t.Fatalf("%s %s: %v", tc.method, tc.path, zErr)
			}

			plain, _ := io.ReadAll(zr)
			body = string(plain)
		}

		if rec.Code != tc.status || !strings.Contains(body, tc.body) ||
			rec.Header().Get("Cache-Control") != tc.cache ||
			rec.Header().Get("Content-Encoding") != tc.encoding ||
			(tc.contentType != "" && rec.Header().Get("Content-Type") != tc.contentType) {
			t.Errorf("%s %s (%q): %d %q %v", tc.method, tc.path, tc.accept, rec.Code, body, rec.Header())
		}
	}
}

func TestHandlerNeedsShell(t *testing.T) {
	if _, err := Handler(fstest.MapFS{}, http.NotFoundHandler(), "127.0.0.1:4567"); err == nil {
		t.Fatal("built a console without its shell page")
	}
}
