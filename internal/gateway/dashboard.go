package gateway

import (
	"embed"
	"net/http"
)

//go:embed dashboard
var dashboard embed.FS

// dashboardFiles are the dashboard's paths under /_tinystack/ and their
// content types.
var dashboardFiles = map[string]string{
	"":        "text/html; charset=utf-8",
	"app.js":  "text/javascript; charset=utf-8",
	"app.css": "text/css; charset=utf-8",
}

// serveDashboard serves one embedded dashboard file. The page loads only its
// own script and style and talks only to TinyStack's own endpoints.
func serveDashboard(name, contentType string) http.HandlerFunc {
	file := name
	if file == "" {
		file = "index.html"
	}

	body, err := dashboard.ReadFile("dashboard/" + file)
	if err != nil {
		panic(err)
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		h := w.Header()
		h.Set("Content-Type", contentType)
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'")
		_, _ = w.Write(body)
	}
}
