// Package console serves the web console (ui/) on its own listener: the built
// single-page app, and TinyStack's own /_tinystack/ endpoints that it reads.
package console

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// Handler serves files, the console's build, and sends /_tinystack/ paths to
// api. Paths that name no file are the app's routes and get its shell page.
func Handler(files fs.FS, api http.Handler) (http.Handler, error) {
	shell, err := fs.ReadFile(files, "_shell.html")
	if err != nil {
		return nil, err
	}

	static := http.FileServerFS(files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_tinystack/") {
			api.ServeHTTP(w, r)
			return
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if info, statErr := fs.Stat(files, name); name != "" && statErr == nil && !info.IsDir() {
			// Vite fingerprints everything under assets/.
			if strings.HasPrefix(name, "assets/") {
				h.Set("Cache-Control", "public, max-age=31536000, immutable")
			}

			static.ServeHTTP(w, r)
			return
		}

		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		if r.Method == http.MethodGet {
			_, _ = w.Write(shell)
		}
	}), nil
}
