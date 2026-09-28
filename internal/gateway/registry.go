package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/aws/sigv4"
)

const (
	// maxManifestBody is the largest manifest a registry client may send.
	maxManifestBody = 4 << 20
	// maxBlobBody is the largest blob chunk a registry client may send.
	maxBlobBody = 4 << 30
)

// Registry serves the Docker Registry HTTP API (the OCI distribution API)
// that `docker login`, `docker push`, and `docker pull` use, as ECR does.
// Every method but Authorize runs in the context's account and region.
type Registry interface {
	// Authorize returns the account and region a client's Basic credentials
	// act in, and whether they are valid.
	Authorize(ctx context.Context, user, password string) (request.Scope, bool, error)
	Blob(ctx context.Context, repo, digest string, data bool) (int64, []byte, error)
	StartUpload(ctx context.Context, repo string) (string, error)
	WriteUpload(ctx context.Context, repo, id string, first int64, data []byte) (int64, error)
	FinishUpload(ctx context.Context, repo, id, digest string, data []byte) error
	MountBlob(ctx context.Context, repo, from, digest string) (bool, error)
	Manifest(ctx context.Context, repo, ref string, pull bool) ([]byte, string, string, error)
	PutManifest(ctx context.Context, repo, ref, mediaType string, body []byte) (string, error)
	DeleteManifest(ctx context.Context, repo, ref string) error
	Tags(ctx context.Context, repo string) ([]string, error)
}

// UseRegistry serves the registry API under /v2/ to requests that are not
// SigV4-signed. Anonymous S3 requests to a bucket named "v2" are then the
// registry's. Call it before serving requests.
func (s *Server) UseRegistry(registry Registry) {
	s.registry = registry
}

// registryErrors are the distribution API's codes and statuses for ECR's
// errors.
var registryErrors = map[string]struct {
	code   string
	status int
}{
	"RepositoryNotFoundException":       {"NAME_UNKNOWN", http.StatusNotFound},
	"ImageNotFoundException":            {"MANIFEST_UNKNOWN", http.StatusNotFound},
	"LayersNotFoundException":           {"BLOB_UNKNOWN", http.StatusNotFound},
	"UploadNotFoundException":           {"BLOB_UPLOAD_UNKNOWN", http.StatusNotFound},
	"InvalidLayerException":             {"DIGEST_INVALID", http.StatusBadRequest},
	"EmptyUploadException":              {"BLOB_UPLOAD_INVALID", http.StatusBadRequest},
	"InvalidLayerPartException":         {"BLOB_UPLOAD_INVALID", http.StatusRequestedRangeNotSatisfiable},
	"ImageDigestDoesNotMatchException":  {"DIGEST_INVALID", http.StatusBadRequest},
	"ImageTagAlreadyExistsException":    {"TAG_INVALID", http.StatusBadRequest},
	"ReferencedImagesNotFoundException": {"MANIFEST_BLOB_UNKNOWN", http.StatusBadRequest},
	"InvalidParameterException":         {"UNSUPPORTED", http.StatusBadRequest},
}

// serveRegistry serves a registry API request, reporting whether the request
// was one.
func (s *Server) serveRegistry(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if s.registry == nil || path != "/v2" && !strings.HasPrefix(path, "/v2/") {
		return false
	}

	// Registry clients send Basic credentials; SigV4 requests are S3's.
	if strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-") || sigv4.Presigned(r.URL.Query()) {
		return false
	}

	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	user, password, _ := r.BasicAuth()
	scope, ok, err := s.registry.Authorize(r.Context(), user, password)
	if err != nil {
		s.registryFail(w, r, err, "")
		return true
	}

	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="TinyStack ECR"`)
		writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "Get a password with "+
			"`aws ecr get-login-password` and log in with the user name AWS.")
		return true
	}

	ctx := request.WithMetadata(r.Context(), request.Metadata{
		Scope: scope, Service: "ecr", Protocol: "registry", Endpoint: "http://" + r.Host,
	})
	rest := strings.TrimPrefix(strings.TrimPrefix(path, "/v2"), "/")
	switch {
	case rest == "":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	case strings.HasSuffix(rest, "/tags/list"):
		s.serveTags(ctx, w, r, strings.TrimSuffix(rest, "/tags/list"))
	case strings.Contains(rest, "/manifests/"):
		i := strings.LastIndex(rest, "/manifests/")
		s.serveManifest(ctx, w, r, rest[:i], rest[i+len("/manifests/"):])
	case strings.Contains(rest, "/blobs/uploads"):
		i := strings.LastIndex(rest, "/blobs/uploads")
		s.serveUpload(ctx, w, r, rest[:i], strings.TrimPrefix(rest[i+len("/blobs/uploads"):], "/"))
	case strings.Contains(rest, "/blobs/"):
		i := strings.LastIndex(rest, "/blobs/")
		s.serveBlob(ctx, w, r, rest[:i], rest[i+len("/blobs/"):])
	default:
		writeRegistryError(w, http.StatusNotFound, "NAME_UNKNOWN", "Unknown registry path")
	}

	return true
}

func writeRegistryError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]any{"errors": []map[string]string{{"code": code, "message": message}}})
	_, _ = w.Write(body)
}

// registryFail writes err as a registry error. kind is "manifests" when a
// manifest was being read or written, for the codes that differ.
func (s *Server) registryFail(w http.ResponseWriter, r *http.Request, err error, kind string) {
	var e *awserror.Error
	if !errors.As(err, &e) {
		s.logger.Error("registry", "method", r.Method, "path", r.URL.Path, "error", err)
		writeRegistryError(w, http.StatusInternalServerError, "UNKNOWN", "internal error")
		return
	}

	mapped, ok := registryErrors[e.Code]
	switch {
	case !ok:
		mapped.code, mapped.status = "UNKNOWN", http.StatusInternalServerError
	case kind == "manifests" && e.Code == "LayersNotFoundException":
		mapped.code, mapped.status = "MANIFEST_BLOB_UNKNOWN", http.StatusBadRequest
	case kind == "manifests" && e.Code == "InvalidParameterException":
		mapped.code = "MANIFEST_INVALID"
	}

	if r.Method == http.MethodHead {
		w.WriteHeader(mapped.status)
		return
	}

	writeRegistryError(w, mapped.status, mapped.code, e.Message)
}

func (s *Server) serveTags(ctx context.Context, w http.ResponseWriter, r *http.Request, repo string) {
	if r.Method != http.MethodGet {
		writeRegistryError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "Tags are only listed")
		return
	}

	tags, err := s.registry.Tags(ctx, repo)
	if err != nil {
		s.registryFail(w, r, err, "")
		return
	}

	body, _ := json.Marshal(map[string]any{"name": repo, "tags": append([]string{}, tags...)})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (s *Server) serveManifest(ctx context.Context, w http.ResponseWriter, r *http.Request, repo, ref string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		body, mediaType, digest, err := s.registry.Manifest(ctx, repo, ref, r.Method == http.MethodGet)
		if err != nil {
			s.registryFail(w, r, err, "manifests")
			return
		}

		w.Header().Set("Content-Type", mediaType)
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodGet {
			// #nosec G705 -- manifests are stored only with their JSON media types.
			_, _ = w.Write(body)
		}
	case http.MethodPut:
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxManifestBody))
		if err != nil {
			writeRegistryError(w, http.StatusRequestEntityTooLarge, "SIZE_INVALID", "The manifest is too large")
			return
		}

		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		digest, err := s.registry.PutManifest(ctx, repo, ref, mediaType, body)
		if err != nil {
			s.registryFail(w, r, err, "manifests")
			return
		}

		w.Header().Set("Location", "/v2/"+repo+"/manifests/"+digest)
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusCreated)
	case http.MethodDelete:
		if err := s.registry.DeleteManifest(ctx, repo, ref); err != nil {
			s.registryFail(w, r, err, "manifests")
			return
		}

		w.WriteHeader(http.StatusAccepted)
	default:
		writeRegistryError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "Unsupported manifest method")
	}
}

func (s *Server) serveBlob(ctx context.Context, w http.ResponseWriter, r *http.Request, repo, digest string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeRegistryError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "Blobs are deleted with their repository")
		return
	}

	size, body, err := s.registry.Blob(ctx, repo, digest, r.Method == http.MethodGet)
	if err != nil {
		s.registryFail(w, r, err, "")
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	if r.Method == http.MethodGet {
		// #nosec G705 -- blobs are served as application/octet-stream.
		_, _ = w.Write(body)
	}
}

// uploadAccepted reports an upload's progress.
func uploadAccepted(w http.ResponseWriter, repo, id string, size int64, status int) {
	w.Header().Set("Location", "/v2/"+repo+"/blobs/uploads/"+id)
	w.Header().Set("Docker-Upload-UUID", id)
	w.Header().Set("Range", "0-"+strconv.FormatInt(max(size-1, 0), 10))
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
}

func blobCreated(w http.ResponseWriter, repo, digest string) {
	w.Header().Set("Location", "/v2/"+repo+"/blobs/"+digest)
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) serveUpload(ctx context.Context, w http.ResponseWriter, r *http.Request, repo, id string) {
	query := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && id == "":
		s.startUpload(ctx, w, r, repo)
	case r.Method == http.MethodPatch && id != "":
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBlobBody))
		if err != nil {
			writeRegistryError(w, http.StatusRequestEntityTooLarge, "SIZE_INVALID", "The chunk is too large")
			return
		}

		first := int64(-1)
		if start, _, ok := strings.Cut(r.Header.Get("Content-Range"), "-"); ok {
			if first, err = strconv.ParseInt(strings.TrimPrefix(start, "bytes="), 10, 64); err != nil {
				writeRegistryError(w, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID",
					"Invalid Content-Range")
				return
			}
		}

		size, err := s.registry.WriteUpload(ctx, repo, id, first, data)
		if err != nil {
			s.registryFail(w, r, err, "")
			return
		}

		uploadAccepted(w, repo, id, size, http.StatusAccepted)
	case r.Method == http.MethodPut && id != "":
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBlobBody))
		if err != nil {
			writeRegistryError(w, http.StatusRequestEntityTooLarge, "SIZE_INVALID", "The chunk is too large")
			return
		}

		digest := query.Get("digest")
		if err = s.registry.FinishUpload(ctx, repo, id, digest, data); err != nil {
			s.registryFail(w, r, err, "")
			return
		}

		blobCreated(w, repo, digest)
	case r.Method == http.MethodGet && id != "":
		size, err := s.registry.WriteUpload(ctx, repo, id, -1, nil)
		if err != nil {
			s.registryFail(w, r, err, "")
			return
		}

		uploadAccepted(w, repo, id, size, http.StatusNoContent)
	default:
		writeRegistryError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "Unsupported upload method")
	}
}

// startUpload mounts a blob from another repository, stores a monolithic
// upload, or starts a chunked one.
func (s *Server) startUpload(ctx context.Context, w http.ResponseWriter, r *http.Request, repo string) {
	query := r.URL.Query()
	if digest, from := query.Get("mount"), query.Get("from"); digest != "" && from != "" {
		mounted, err := s.registry.MountBlob(ctx, repo, from, digest)
		if err != nil {
			s.registryFail(w, r, err, "")
			return
		}

		if mounted {
			blobCreated(w, repo, digest)
			return
		}
	}

	id, err := s.registry.StartUpload(ctx, repo)
	if err != nil {
		s.registryFail(w, r, err, "")
		return
	}

	digest := query.Get("digest")
	if digest == "" {
		uploadAccepted(w, repo, id, 0, http.StatusAccepted)
		return
	}

	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBlobBody))
	if err != nil {
		writeRegistryError(w, http.StatusRequestEntityTooLarge, "SIZE_INVALID", "The blob is too large")
		return
	}

	if err = s.registry.FinishUpload(ctx, repo, id, digest, data); err != nil {
		s.registryFail(w, r, err, "")
		return
	}

	blobCreated(w, repo, digest)
}
