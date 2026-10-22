// Package console serves the web console (ui/) on its own listener: the built
// single-page app, and TinyStack's own /_tinystack/ endpoints that it reads.
package console

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

// Handler serves files, the console's build with every file stored gzipped as
// name.gz, and sends /_tinystack/ paths to api. In the console build, /mcp is
// the MCP server for the console listening on address. Paths that name no file
// are the app's routes and get its shell page.
func Handler(files fs.FS, api http.Handler, address string) (http.Handler, error) {
	shell, err := gunzip(files, "_shell.html.gz")
	if err != nil {
		return nil, err
	}

	mcp := mcpHandler(api, address)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_tinystack/") {
			api.ServeHTTP(w, r)
			return
		}

		if mcp != nil && r.URL.Path == "/mcp" {
			mcp.ServeHTTP(w, r)
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
		compressed := name + ".gz"
		if info, statErr := fs.Stat(files, compressed); name != "" && statErr == nil && !info.IsDir() {
			// Vite fingerprints everything under assets/.
			if strings.HasPrefix(name, "assets/") {
				h.Set("Cache-Control", "public, max-age=31536000, immutable")
			}

			h.Set("Content-Type", cmp.Or(mime.TypeByExtension(path.Ext(name)), "application/octet-stream"))
			h.Add("Vary", "Accept-Encoding")
			if acceptsGzip(r) {
				h.Set("Content-Encoding", "gzip")
				// #nosec G703 -- files is the console build's fs.FS and the cleaned name exists in it.
				http.ServeFileFS(w, r, files, compressed)
				return
			}

			body, gunzipErr := gunzip(files, compressed)
			if gunzipErr != nil {
				http.Error(w, gunzipErr.Error(), http.StatusInternalServerError)
				return
			}

			http.ServeContent(w, r, name, info.ModTime(), bytes.NewReader(body))
			return
		}

		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		if r.Method == http.MethodGet {
			_, _ = w.Write(shell)
		}
	}), nil
}

// acceptsGzip reports whether the request's Accept-Encoding takes gzip.
func acceptsGzip(r *http.Request) bool {
	for _, field := range r.Header.Values("Accept-Encoding") {
		for coding := range strings.SplitSeq(field, ",") {
			name, params, _ := strings.Cut(coding, ";")
			if name = strings.TrimSpace(name); !strings.EqualFold(name, "gzip") && name != "*" {
				continue
			}

			q, weighted := strings.CutPrefix(strings.TrimSpace(params), "q=")
			return !weighted || strings.Trim(q, "0.") != ""
		}
	}

	return false
}

func gunzip(files fs.FS, name string) ([]byte, error) {
	f, err := files.Open(name)
	if err != nil {
		return nil, err
	}

	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}

	return io.ReadAll(zr)
}
