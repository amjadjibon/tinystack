package gateway

import (
	"context"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/s3"
)

// UseS3Website connects the public website endpoint before the REST API dispatcher.
func (s *Server) UseS3Website(fn func(context.Context, *s3.WebsiteRequest) (*s3.WebsiteResponse, error)) {
	s.s3Website = fn
}

func s3WebsiteHost(host string) (bucket, region string) {
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}

	base, ok := strings.CutSuffix(strings.ToLower(host), ".localhost")
	if !ok {
		return "", ""
	}

	for _, separator := range []string{".s3-website.", ".s3-website-"} {
		pos := strings.LastIndex(base, separator)
		if pos > 0 {
			bucket, region := base[:pos], base[pos+len(separator):]
			if region != "" && !strings.Contains(region, ".") {
				return bucket, region
			}
		}
	}

	return "", ""
}

func (s *Server) serveS3Website(w http.ResponseWriter, r *http.Request) bool {
	bucket, region := s3WebsiteHost(r.Host)
	if s.s3Website == nil || bucket == "" {
		return false
	}

	start := time.Now()
	wrapped := &capturingWriter{ResponseWriter: w}
	id := rand.Text()
	key := strings.TrimPrefix(r.URL.Path, "/")
	c := &call{
		meta: request.Metadata{
			Scope: request.Scope{Region: region}, Service: "s3", Operation: opGetObject, RequestID: id,
		},
		labels: map[string]string{"Bucket": bucket, "Key": key},
	}
	defer func() {
		rec := &Record{
			Time: start.UTC(), Service: "s3", Region: region, RequestID: id, Status: wrapped.status,
			DurationMS: float64(time.Since(start).Microseconds()) / 1000,
		}
		s.recordS3Access(r, c, rec, wrapped)
	}()

	w = wrapped
	w.Header().Set("x-amz-request-id", id)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeWebsiteError(w, r, http.StatusMethodNotAllowed)
		return true
	}

	scheme := httpScheme
	if r.TLS != nil {
		scheme = httpsScheme
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}

	out, err := s.s3Website(r.Context(), &s3.WebsiteRequest{
		Bucket: bucket, Region: region, Key: key, Host: r.Host, Scheme: scheme, RemoteIP: host,
		Head: r.Method == http.MethodHead,
	})
	switch {
	case err != nil:
		s.logger.Error("S3 website request failed", "bucket", bucket, "error", err)
		writeWebsiteError(w, r, http.StatusInternalServerError)
	case out.Location != "":
		http.Redirect(w, r, out.Location, out.Status)
	case out.ContentType == "":
		writeWebsiteError(w, r, out.Status)
	default:
		w.Header().Set("Content-Type", out.ContentType)
		w.Header().Set("Content-Length", strconv.FormatInt(out.Length, 10))
		w.Header().Set("ETag", out.ETag)
		w.Header().Set("Last-Modified", out.LastModified.Format(http.TimeFormat))
		w.WriteHeader(out.Status)
		if out.Body != nil {
			defer func() { _ = out.Body.Close() }()
			_, _ = io.Copy(w, out.Body)
		}
	}

	return true
}

func writeWebsiteError(w http.ResponseWriter, r *http.Request, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, "<html><body><h1>"+http.StatusText(status)+"</h1></body></html>")
	}
}
