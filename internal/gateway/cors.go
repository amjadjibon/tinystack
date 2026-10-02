package gateway

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/restxml"
)

// CORS returns the response headers bucket's CORS configuration gives a
// request from origin with method and, for a preflight, the headers it asks to
// send, and whether a rule allows the request.
type CORS func(ctx context.Context, bucket, origin, method string, headers []string) (map[string]string, bool, error)

// UseCORS answers S3 CORS preflights (OPTIONS requests) and adds CORS headers
// to S3 responses through cors. Call it before serving requests.
func (s *Server) UseCORS(cors CORS) {
	s.cors = cors
}

// s3Bucket is the bucket an S3 request names: the virtual host's, or the first
// path segment's.
func s3Bucket(r *http.Request) string {
	if bucket := virtualBucket(r.Host); bucket != "" {
		return bucket
	}

	first, _, _ := strings.Cut(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	bucket, err := url.PathUnescape(first)
	if err != nil {
		return ""
	}

	return bucket
}

// servePreflight answers an unsigned CORS preflight to an S3 bucket, reporting
// whether the request was one.
func (s *Server) servePreflight(w http.ResponseWriter, r *http.Request) bool {
	origin, method := r.Header.Get("Origin"), r.Header.Get("Access-Control-Request-Method")
	bucket := s3Bucket(r)
	if s.cors == nil || r.Method != http.MethodOptions || origin == "" || method == "" || bucket == "" {
		return false
	}

	var headers []string
	for _, h := range strings.Split(r.Header.Get("Access-Control-Request-Headers"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			headers = append(headers, h)
		}
	}

	allowed, ok, err := s.cors(r.Context(), bucket, origin, method, headers)
	switch {
	case err != nil:
		s.logger.Error("CORS preflight", "bucket", bucket, "error", err)
		restxml.WriteError(w, awserror.From(err), "", "", false)
	case !ok:
		restxml.WriteError(w, &awserror.Error{
			Code: "AccessForbidden", StatusCode: http.StatusForbidden,
			Message: "CORSResponse: This CORS request is not allowed. This is usually because the evalution of " +
				"Origin, request method / Access-Control-Request-Method or Access-Control-Request-Headers are not " +
				"whitelisted by the resource's CORS spec.",
		}, "", "", false)
	default:
		for k, v := range allowed {
			w.Header().Set(k, v)
		}

		w.WriteHeader(http.StatusOK)
	}

	return true
}

// addCORS sets the CORS headers of an S3 request sent from a browser origin.
func (s *Server) addCORS(w http.ResponseWriter, r *http.Request, c *call) {
	origin := r.Header.Get("Origin")
	if s.cors == nil || origin == "" || c.service == nil || c.service.SigningName != "s3" || c.labels["Bucket"] == "" {
		return
	}

	allowed, ok, err := s.cors(r.Context(), c.labels["Bucket"], origin, r.Method, nil)
	if err != nil {
		s.logger.Error("CORS", "bucket", c.labels["Bucket"], "error", err)
	}

	if ok {
		for k, v := range allowed {
			w.Header().Set(k, v)
		}
	}
}
