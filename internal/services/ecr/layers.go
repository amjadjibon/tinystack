package ecr

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecr"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	// partSize is the part size InitiateLayerUpload suggests.
	partSize = 10 << 20
	// downloadValidity is how long a GetDownloadUrlForLayer URL works.
	downloadValidity = time.Hour
	available        = "AVAILABLE"
)

// layer is a blob's record; its bytes are under blob/.
type layer struct {
	Size int64
}

// upload is an unfinished layer upload's record; its bytes so far are under
// upload-data/.
type upload struct {
	Started time.Time
	Size    int64
}

var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func newUploadID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

func layerKey(scope request.Scope, repo, digest string) string {
	return owned(scope, "layer", repo) + digest
}

func blobKey(scope request.Scope, repo, digest string) string {
	return owned(scope, "blob", repo) + digest
}

func uploadKey(scope request.Scope, repo, id string) string { return owned(scope, "upload", repo) + id }

func uploadDataKey(scope request.Scope, repo, id string) string {
	return owned(scope, "upload-data", repo) + id
}

func layersNotFound(digests ...string) error {
	return awserror.New("LayersNotFoundException", fmt.Sprintf("The following layers were not found: %v", digests))
}

func uploadNotFound(id string) error {
	return awserror.New("UploadNotFoundException", "The upload with id '"+id+"' does not exist")
}

// startUpload begins a layer upload in r and returns its ID.
func (s *Service) startUpload(tx storage.WriteTx, scope request.Scope, r *repository) (string, error) {
	id := newUploadID()
	tx.Put(uploadDataKey(scope, r.Name, id), nil)
	return id, storage.Write(tx, uploadKey(scope, r.Name, id), upload{Started: s.now().UTC()})
}

// appendUpload adds data to an upload at first, which must be the upload's
// size, or at its end when first is negative, and returns the new size.
func appendUpload(tx storage.WriteTx, scope request.Scope, r *repository, id string, first int64, data []byte) (
	int64, error,
) {
	u, ok, err := storage.Read[upload](tx, uploadKey(scope, r.Name, id))
	if err != nil {
		return 0, err
	}

	if !ok {
		return 0, uploadNotFound(id)
	}

	if first >= 0 && first != u.Size {
		return 0, awserror.New("InvalidLayerPartException", fmt.Sprintf(
			"The part starts at byte %d, but %d bytes were received so far", first, u.Size))
	}

	current, _ := tx.Get(uploadDataKey(scope, r.Name, id))
	tx.Put(uploadDataKey(scope, r.Name, id), append(current, data...))
	u.Size += int64(len(data))
	return u.Size, storage.Write(tx, uploadKey(scope, r.Name, id), u)
}

// completeUpload stores an upload as the layer digest names. A layer that
// exists fails with LayerAlreadyExistsException unless exists is fine.
func completeUpload(tx storage.WriteTx, scope request.Scope, r *repository, id, digest string, existsOK bool) error {
	if !digestPattern.MatchString(digest) {
		return awserror.New("InvalidLayerException", "Invalid layer digest: "+digest)
	}

	u, ok, err := storage.Read[upload](tx, uploadKey(scope, r.Name, id))
	if err != nil {
		return err
	}

	if !ok {
		return uploadNotFound(id)
	}

	data, _ := tx.Get(uploadDataKey(scope, r.Name, id))
	if u.Size == 0 {
		return awserror.New("EmptyUploadException", "The upload with id '"+id+"' contains no data")
	}

	if got := digestOf(data); got != digest {
		return awserror.New("InvalidLayerException", "The layer's digest is "+got+", not "+digest)
	}

	tx.Delete(uploadKey(scope, r.Name, id))
	tx.Delete(uploadDataKey(scope, r.Name, id))
	if _, exists := tx.Get(layerKey(scope, r.Name, digest)); exists {
		if existsOK {
			return nil
		}

		return awserror.New("LayerAlreadyExistsException", "The layer with digest '"+digest+"' already exists")
	}

	tx.Put(blobKey(scope, r.Name, digest), data)
	return storage.Write(tx, layerKey(scope, r.Name, digest), layer{Size: u.Size})
}

// findLayer returns the layer digest names in r.
func findLayer(tx storage.ReadTx, scope request.Scope, r *repository, digest string) (*layer, bool, error) {
	return storage.Read[layer](tx, layerKey(scope, r.Name, digest))
}

func (s *Service) BatchCheckLayerAvailability(
	ctx context.Context,
	in *api.BatchCheckLayerAvailabilityRequest,
) (*api.BatchCheckLayerAvailabilityResponse, error) {
	if len(in.LayerDigests) < 1 || len(in.LayerDigests) > 100 {
		return nil, invalidParameter("layerDigests must name 1 to 100 layers.")
	}

	out := &api.BatchCheckLayerAvailabilityResponse{Layers: []api.Layer{}, Failures: []api.LayerFailure{}}
	err := s.view(ctx, in.RegistryId, in.RepositoryName,
		func(tx storage.ReadTx, m *request.Metadata, r *repository) error {
			for _, digest := range in.LayerDigests {
				if !digestPattern.MatchString(digest) {
					out.Failures = append(out.Failures, api.LayerFailure{
						LayerDigest: new(digest), FailureCode: new("InvalidLayerDigest"),
						FailureReason: new("Invalid layer digest"),
					})
					continue
				}

				l, ok, err := findLayer(tx, m.Scope, r, digest)
				if err != nil {
					return err
				}

				if !ok {
					out.Failures = append(out.Failures, api.LayerFailure{
						LayerDigest: new(digest), FailureCode: new("MissingLayerDigest"),
						FailureReason: new("Requested layer digest does not exist"),
					})
					continue
				}

				out.Layers = append(out.Layers, api.Layer{
					LayerDigest: new(digest), LayerAvailability: new(available), LayerSize: new(l.Size),
				})
			}

			return nil
		})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) InitiateLayerUpload(
	ctx context.Context,
	in *api.InitiateLayerUploadRequest,
) (*api.InitiateLayerUploadResponse, error) {
	var id string
	err := s.update(ctx, in.RegistryId, in.RepositoryName,
		func(tx storage.WriteTx, m *request.Metadata, r *repository) error {
			var err error
			id, err = s.startUpload(tx, m.Scope, r)
			return err
		})
	if err != nil {
		return nil, err
	}

	return &api.InitiateLayerUploadResponse{UploadId: new(id), PartSize: new(int64(partSize))}, nil
}

func (s *Service) UploadLayerPart(
	ctx context.Context,
	in *api.UploadLayerPartRequest,
) (*api.UploadLayerPartResponse, error) {
	first, last := common.Value(in.PartFirstByte), common.Value(in.PartLastByte)
	if in.UploadId == nil || first < 0 || len(in.LayerPartBlob) == 0 ||
		last != first+int64(len(in.LayerPartBlob))-1 {
		return nil, awserror.New("InvalidLayerPartException",
			"partFirstByte and partLastByte must bound the part's bytes")
	}

	var out *api.UploadLayerPartResponse
	err := s.update(ctx, in.RegistryId, in.RepositoryName,
		func(tx storage.WriteTx, m *request.Metadata, r *repository) error {
			size, err := appendUpload(tx, m.Scope, r, *in.UploadId, first, in.LayerPartBlob)
			if err != nil {
				return err
			}

			out = &api.UploadLayerPartResponse{
				RegistryId: new(m.AccountID), RepositoryName: new(r.Name), UploadId: in.UploadId,
				LastByteReceived: new(size - 1),
			}
			return nil
		})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) CompleteLayerUpload(
	ctx context.Context,
	in *api.CompleteLayerUploadRequest,
) (*api.CompleteLayerUploadResponse, error) {
	if len(in.LayerDigests) != 1 || in.UploadId == nil {
		return nil, invalidParameter("CompleteLayerUpload takes an uploadId and one layer digest.")
	}

	var out *api.CompleteLayerUploadResponse
	err := s.update(ctx, in.RegistryId, in.RepositoryName,
		func(tx storage.WriteTx, m *request.Metadata, r *repository) error {
			if err := completeUpload(tx, m.Scope, r, *in.UploadId, in.LayerDigests[0], false); err != nil {
				return err
			}

			out = &api.CompleteLayerUploadResponse{
				RegistryId: new(m.AccountID), RepositoryName: new(r.Name), UploadId: in.UploadId,
				LayerDigest: new(in.LayerDigests[0]),
			}
			return nil
		})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// GetDownloadUrlForLayer returns a registry URL for the layer whose user
// information carries a password that works for an hour.
func (s *Service) GetDownloadUrlForLayer(
	ctx context.Context,
	in *api.GetDownloadUrlForLayerRequest,
) (*api.GetDownloadUrlForLayerResponse, error) {
	digest := common.Value(in.LayerDigest)
	var m *request.Metadata
	err := s.view(ctx, in.RegistryId, in.RepositoryName,
		func(tx storage.ReadTx, meta *request.Metadata, r *repository) error {
			m = meta
			if _, ok, err := findLayer(tx, meta.Scope, r, digest); err != nil || !ok {
				return common.ErrorsOr(err, layersNotFound(digest))
			}

			return nil
		})
	if err != nil {
		return nil, err
	}

	password, _, err := s.issue(ctx, m.Scope, downloadValidity)
	if err != nil {
		return nil, err
	}

	base, err := url.Parse(m.Endpoint)
	if err != nil {
		return nil, err
	}

	base.User = url.UserPassword(registryUser, password)
	base.Path = "/v2/" + *in.RepositoryName + "/blobs/" + digest
	return &api.GetDownloadUrlForLayerResponse{DownloadUrl: new(base.String()), LayerDigest: new(digest)}, nil
}
