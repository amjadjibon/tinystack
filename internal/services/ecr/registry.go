package ecr

import (
	"context"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecr"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// These methods serve the Docker Registry HTTP API that the gateway speaks, in
// the account and region of the client's password. They share the ECR API's
// records and errors.

// Blob returns the size of a layer in repo and, if data is set, its bytes.
func (s *Service) Blob(ctx context.Context, repo, digest string, data bool) (size int64, body []byte, err error) {
	err = s.view(ctx, nil, &repo, func(tx storage.ReadTx, m *request.Metadata, r *repository) error {
		l, ok, readErr := findLayer(tx, m.Scope, r, digest)
		if readErr != nil || !ok {
			return errorsOr(readErr, layersNotFound(digest))
		}

		size = l.Size
		if data {
			body, _ = tx.Get(blobKey(m.Scope, r.Name, digest))
		}

		return nil
	})

	return size, body, err
}

// StartUpload begins a blob upload in repo and returns its ID.
func (s *Service) StartUpload(ctx context.Context, repo string) (id string, err error) {
	err = s.update(ctx, nil, &repo, func(tx storage.WriteTx, m *request.Metadata, r *repository) error {
		id, err = s.startUpload(tx, m.Scope, r)
		return err
	})

	return id, err
}

// WriteUpload adds data to an upload at first, or at its end when first is
// negative, and returns the upload's size.
func (s *Service) WriteUpload(ctx context.Context, repo, id string, first int64, data []byte) (size int64, err error) {
	err = s.update(ctx, nil, &repo, func(tx storage.WriteTx, m *request.Metadata, r *repository) error {
		size, err = appendUpload(tx, m.Scope, r, id, first, data)
		return err
	})

	return size, err
}

// FinishUpload adds data to an upload and stores it as the blob digest names.
// A blob that exists is kept.
func (s *Service) FinishUpload(ctx context.Context, repo, id, digest string, data []byte) error {
	return s.update(ctx, nil, &repo, func(tx storage.WriteTx, m *request.Metadata, r *repository) error {
		if len(data) > 0 {
			if _, err := appendUpload(tx, m.Scope, r, id, -1, data); err != nil {
				return err
			}
		}

		return completeUpload(tx, m.Scope, r, id, digest, true)
	})
}

// MountBlob copies a blob from another repository of the same registry into
// repo and reports whether from had it.
func (s *Service) MountBlob(ctx context.Context, repo, from, digest string) (mounted bool, err error) {
	if !digestPattern.MatchString(digest) || !repoName.MatchString(from) {
		return false, nil
	}

	err = s.update(ctx, nil, &repo, func(tx storage.WriteTx, m *request.Metadata, r *repository) error {
		l, ok, readErr := read[layer](tx, layerKey(m.Scope, from, digest))
		if readErr != nil || !ok {
			return readErr
		}

		if _, exists := tx.Get(repoKey(m.Scope, from)); !exists {
			return nil
		}

		data, _ := tx.Get(blobKey(m.Scope, from, digest))
		tx.Put(blobKey(m.Scope, r.Name, digest), data)
		mounted = true
		return write(tx, layerKey(m.Scope, r.Name, digest), l)
	})

	return mounted, err
}

// reference is an image ID from a registry path's tag or digest.
func reference(ref string) api.ImageIdentifier {
	if strings.Contains(ref, ":") {
		return api.ImageIdentifier{ImageDigest: &ref}
	}

	return api.ImageIdentifier{ImageTag: &ref}
}

func imageNotFound(r *repository, ref string) error {
	return awserror.New("ImageNotFoundException", "The image '"+ref+"' does not exist in the repository '"+
		r.Name+"'")
}

// Manifest returns the image manifest ref (a tag or digest) names in repo,
// its media type, and its digest, recording a pull if pull is set.
func (s *Service) Manifest(ctx context.Context, repo, ref string, pull bool) (body []byte, mediaType, digest string,
	err error,
) {
	err = s.update(ctx, nil, &repo, func(tx storage.WriteTx, m *request.Metadata, r *repository) error {
		img, failure, findErr := findImage(tx, m.Scope, r, reference(ref))
		if findErr != nil {
			return findErr
		}

		if failure != nil {
			return imageNotFound(r, ref)
		}

		body, mediaType, digest = []byte(img.Manifest), img.MediaType, img.Digest
		if pull {
			return s.pulled(tx, m.Scope, r, img)
		}

		return nil
	})

	return body, mediaType, digest, err
}

// PutManifest stores a manifest in repo under ref, a tag or the manifest's
// digest, and returns its digest.
func (s *Service) PutManifest(ctx context.Context, repo, ref, mediaType string, body []byte) (digest string,
	err error,
) {
	p := &push{Manifest: body, MediaType: mediaType, Repeat: true}
	if id := reference(ref); id.ImageDigest != nil {
		p.Digest = ref
	} else {
		p.Tag = ref
	}

	err = s.update(ctx, nil, &repo, func(tx storage.WriteTx, m *request.Metadata, r *repository) error {
		img, putErr := s.putImage(tx, m.Scope, r, p)
		if putErr != nil {
			return putErr
		}

		digest = img.Digest
		return nil
	})

	return digest, err
}

// DeleteManifest deletes the image ref names, or only the tag ref when it
// is one of the image's tags.
func (s *Service) DeleteManifest(ctx context.Context, repo, ref string) error {
	return s.update(ctx, nil, &repo, func(tx storage.WriteTx, m *request.Metadata, r *repository) error {
		_, failure, err := untag(tx, m.Scope, r, reference(ref))
		switch {
		case err != nil:
			return err
		case failure != nil && failure.code == failureNotFound:
			return imageNotFound(r, ref)
		case failure != nil:
			return invalidParameter("%s", failure.reason)
		}

		return nil
	})
}

// Tags returns repo's tags in order.
func (s *Service) Tags(ctx context.Context, repo string) (tags []string, err error) {
	err = s.view(ctx, nil, &repo, func(tx storage.ReadTx, m *request.Metadata, r *repository) error {
		tags = suffixes(tx, owned(m.Scope, "tag", r.Name))
		return nil
	})

	return slices.Clip(tags), err
}
