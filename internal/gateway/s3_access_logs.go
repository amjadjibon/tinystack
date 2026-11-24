package gateway

import (
	"cmp"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/aws/sigv4"
	"github.com/amjadjibon/tinystack/internal/services/s3"
)

// UseS3AccessLogs connects the S3 server access log recorder.
func (s *Server) UseS3AccessLogs(fn func(context.Context, *s3.AccessRecord) error) {
	s.s3AccessLogs = fn
}

func s3AccessOperation(r *http.Request, c *call) string {
	switch c.meta.Operation {
	case "CopyObject":
		return "REST.COPY.OBJECT"
	case opPostObject:
		return "REST.POST.OBJECT"
	case "ListObjects", "ListObjectsV2":
		return "REST.GET.BUCKET"
	case "ListObjectVersions":
		return "REST.GET.BUCKETVERSIONS"
	case "HeadBucket", "CreateBucket", "DeleteBucket":
		return "REST." + r.Method + ".BUCKET"
	case opPutObject, opGetObject, "HeadObject", "DeleteObject":
		return "REST." + r.Method + ".OBJECT"
	case "UploadPart":
		return "REST.PUT.PART"
	case "CreateMultipartUpload", "CompleteMultipartUpload":
		return "REST.POST.UPLOADS"
	case "AbortMultipartUpload", "ListParts":
		return "REST." + r.Method + ".UPLOAD"
	}

	op := strings.TrimPrefix(c.meta.Operation, "Get")
	op = strings.TrimPrefix(op, "Put")
	op = strings.TrimPrefix(op, "Delete")
	op = strings.TrimPrefix(op, "List")
	return "REST." + r.Method + "." + strings.ToUpper(op)
}

func s3ObjectSize(r *http.Request, w *capturingWriter, c *call) int64 {
	if w.status >= http.StatusBadRequest {
		return -1
	}

	var raw string
	switch c.meta.Operation {
	case opPutObject:
		raw = cmp.Or(r.Header.Get("X-Amz-Decoded-Content-Length"), r.Header.Get("Content-Length"))
	case opGetObject, "HeadObject":
		_, raw, _ = strings.Cut(w.Header().Get("Content-Range"), "/")
		raw = cmp.Or(raw, w.Header().Get("Content-Length"))
	}

	size, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return -1
	}

	return size
}

func (s *Server) recordS3Access(r *http.Request, c *call, rec *Record, w *capturingWriter) {
	bucket := c.labels["Bucket"]
	if s.s3AccessLogs == nil || rec.Service != "s3" || bucket == "" {
		return
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}

	query := r.URL.Query()
	for name := range query {
		if sigv4.QueryParameter(name) || strings.HasPrefix(strings.ToLower(name), "x-amz-") {
			delete(query, name)
		}
	}

	uri := r.URL.EscapedPath()
	if len(query) > 0 {
		uri += "?" + query.Encode()
	}

	a := &s3.AccessRecord{
		Time: rec.Time, Bucket: bucket, Key: c.labels["Key"], Region: rec.Region,
		RemoteIP: host, RequestID: rec.RequestID, Operation: s3AccessOperation(r, c),
		URI: r.Method + " " + uri + " " + r.Proto, ErrorCode: rec.ErrorCode,
		Referer: r.Referer(), UserAgent: r.UserAgent(), Version: r.URL.Query().Get("versionId"),
		HostID: w.Header().Get("x-amz-id-2"), Host: r.Host, Status: rec.Status, Bytes: w.size,
		ObjectSize: s3ObjectSize(r, w, c), TotalMS: int64(rec.DurationMS), TurnaroundMS: -1,
	}
	if !w.firstWrite.IsZero() {
		a.TurnaroundMS = w.firstWrite.Sub(rec.Time).Milliseconds()
	}

	if c.meta.Principal != nil && c.meta.Principal.Kind != request.PrincipalAnonymous {
		a.Requester = c.meta.Principal.ARN
	}

	if auth, signed, err := signature(r); err == nil && signed {
		a.Signature, a.Authentication = "SigV4", "AuthHeader"
		if auth.Expires != 0 {
			a.Authentication = "QueryString"
		}
	}

	if r.TLS != nil {
		a.Cipher = tls.CipherSuiteName(r.TLS.CipherSuite)
		a.TLS = tls.VersionName(r.TLS.Version)
	}

	if err := s.s3AccessLogs(context.WithoutCancel(r.Context()), a); err != nil {
		s.logger.Warn("record S3 server access log", "error", err)
	}
}
