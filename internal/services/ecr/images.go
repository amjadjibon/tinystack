package ecr

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecr"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	dockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	dockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	ociManifest    = "application/vnd.oci.image.manifest.v1+json"
	ociIndex       = "application/vnd.oci.image.index.v1+json"
	// maxManifest is the largest manifest ECR stores.
	maxManifest = 4 << 20

	tagged   = "TAGGED"
	untagged = "UNTAGGED"
	anyTag   = "ANY"

	failureNotFound = "ImageNotFound"
)

// image is an image manifest's record, under image/<escaped repo>/<digest>.
type image struct {
	Pushed            time.Time
	Pulled            time.Time `json:",omitzero"`
	Digest            string
	Manifest          string
	MediaType         string
	ArtifactMediaType string   `json:",omitempty"`
	Subject           string   `json:",omitempty"`
	Tags              []string `json:",omitempty"`
	// References are the digests of the images an index lists.
	References []string `json:",omitempty"`
	Size       int64
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// manifest is the part of an image manifest or index ECR reads.
type manifest struct {
	Config        *descriptor  `json:"config"`
	Subject       *descriptor  `json:"subject"`
	MediaType     string       `json:"mediaType"`
	ArtifactType  string       `json:"artifactType"`
	Layers        []descriptor `json:"layers"`
	Manifests     []descriptor `json:"manifests"`
	SchemaVersion int          `json:"schemaVersion"`
}

var tagPattern = regexp.MustCompile(`^\w[\w.-]{0,299}$`)

func imageKey(scope request.Scope, repo, digest string) string {
	return owned(scope, "image", repo) + digest
}

func tagKey(scope request.Scope, repo, tag string) string { return owned(scope, "tag", repo) + tag }

func isIndex(mediaType string) bool { return mediaType == dockerList || mediaType == ociIndex }

// parseManifest reads a manifest sent with mediaType, which may be empty, and
// returns the media type it has.
func parseManifest(body []byte, mediaType string) (*manifest, string, error) {
	var mf manifest
	if len(body) == 0 || len(body) > maxManifest || json.Unmarshal(body, &mf) != nil {
		return nil, "", invalidParameter("Invalid parameter at 'imageManifest' failed to satisfy constraint: " +
			"'Invalid JSON syntax'")
	}

	if mf.MediaType != "" && mediaType != "" && mf.MediaType != mediaType {
		return nil, "", invalidParameter("The manifest's mediaType %s does not match %s.", mf.MediaType, mediaType)
	}

	switch {
	case mf.MediaType != "":
		mediaType = mf.MediaType
	case mediaType != "":
	case mf.Manifests != nil:
		mediaType = ociIndex
	default:
		mediaType = ociManifest
	}

	if mf.SchemaVersion != 2 ||
		!slices.Contains([]string{dockerManifest, dockerList, ociManifest, ociIndex}, mediaType) {
		return nil, "", invalidParameter("TinyStack supports schema version 2 Docker and OCI image manifests and "+
			"indexes, not %s.", mediaType)
	}

	var refs []descriptor
	switch {
	case isIndex(mediaType):
		refs = mf.Manifests
	case mf.Config == nil:
		return nil, "", invalidParameter("The image manifest has no config.")
	default:
		refs = append([]descriptor{*mf.Config}, mf.Layers...)
	}

	for _, d := range refs {
		if !digestPattern.MatchString(d.Digest) {
			return nil, "", invalidParameter("The manifest references an invalid digest %q.", d.Digest)
		}
	}

	return &mf, mediaType, nil
}

// push is a PutImage from the API or the registry.
type push struct {
	MediaType string
	Tag       string
	Digest    string
	Manifest  []byte
	// Repeat accepts an image that exists, as registry pushes do; the API
	// reports ImageAlreadyExistsException.
	Repeat bool
}

// putImage stores an image manifest in r and tags it.
func (s *Service) putImage(tx storage.WriteTx, scope request.Scope, r *repository, p *push) (*image, error) {
	mf, mediaType, err := parseManifest(p.Manifest, p.MediaType)
	if err != nil {
		return nil, err
	}

	digest := digestOf(p.Manifest)
	if p.Digest != "" && p.Digest != digest {
		return nil, awserror.New("ImageDigestDoesNotMatchException", "The manifest's digest "+digest+
			" does not match "+p.Digest)
	}

	if p.Tag != "" && !tagPattern.MatchString(p.Tag) {
		return nil, invalidParameter("Invalid parameter at 'imageTag' failed to satisfy constraint: 'Invalid tag'")
	}

	img, exists, err := read[image](tx, imageKey(scope, r.Name, digest))
	if err != nil {
		return nil, err
	}

	if !exists {
		if img, err = newImage(tx, scope, r, mf, mediaType, p.Manifest); err != nil {
			return nil, err
		}

		img.Pushed = s.now().UTC()
	}

	changed := !exists
	if p.Tag != "" && !slices.Contains(img.Tags, p.Tag) {
		if moveErr := moveTag(tx, scope, r, p.Tag, digest); moveErr != nil {
			return nil, moveErr
		}

		img.Tags = append(img.Tags, p.Tag)
		slices.Sort(img.Tags)
		changed = true
	}

	if !changed && !p.Repeat {
		return nil, awserror.New("ImageAlreadyExistsException", fmt.Sprintf(
			"Image with digest '%s' and tag '%s' already exists in the repository with name '%s' in registry "+
				"with id '%s'", digest, p.Tag, r.Name, scope.AccountID))
	}

	if writeErr := write(tx, imageKey(scope, r.Name, digest), img); writeErr != nil {
		return nil, writeErr
	}

	if changed {
		if expireErr := s.expire(tx, scope, r); expireErr != nil {
			return nil, expireErr
		}
	}

	return img, nil
}

// newImage checks that the blobs or images a manifest references are in r
// and returns its record.
func newImage(tx storage.ReadTx, scope request.Scope, r *repository, mf *manifest, mediaType string, body []byte) (
	*image, error,
) {
	img := &image{
		Digest:            digestOf(body),
		Manifest:          string(body),
		MediaType:         mediaType,
		ArtifactMediaType: mf.ArtifactType,
	}
	if mf.Subject != nil {
		img.Subject = mf.Subject.Digest
	}

	if isIndex(mediaType) {
		var missing []string
		for _, d := range mf.Manifests {
			child, ok, err := read[image](tx, imageKey(scope, r.Name, d.Digest))
			if err != nil {
				return nil, err
			}

			if !ok {
				missing = append(missing, d.Digest)
				continue
			}

			img.References = append(img.References, d.Digest)
			img.Size += child.Size
		}

		if len(missing) > 0 {
			return nil, awserror.New("ReferencedImagesNotFoundException", fmt.Sprintf(
				"The following referenced images were not found: %v", missing))
		}

		return img, nil
	}

	if img.ArtifactMediaType == "" {
		img.ArtifactMediaType = mf.Config.MediaType
	}

	var missing []string
	for i, d := range append([]descriptor{*mf.Config}, mf.Layers...) {
		l, ok, err := findLayer(tx, scope, r, d.Digest)
		if err != nil {
			return nil, err
		}

		switch {
		case !ok:
			missing = append(missing, d.Digest)
		case i > 0:
			img.Size += l.Size
		}
	}

	if len(missing) > 0 {
		return nil, layersNotFound(missing...)
	}

	return img, nil
}

// moveTag points tag at digest, taking it from the image that has it unless
// r's tags are immutable.
func moveTag(tx storage.WriteTx, scope request.Scope, r *repository, tag, digest string) error {
	if old, ok := tx.Get(tagKey(scope, r.Name, tag)); ok {
		if r.Mutability == immutable {
			return awserror.New("ImageTagAlreadyExistsException", "The image tag '"+tag+
				"' already exists in the '"+r.Name+"' repository and cannot be overwritten because the "+
				"repository is immutable.")
		}

		prev, found, err := read[image](tx, imageKey(scope, r.Name, string(old)))
		if err != nil {
			return err
		}

		if found {
			prev.Tags = slices.DeleteFunc(prev.Tags, func(t string) bool { return t == tag })
			if writeErr := write(tx, imageKey(scope, r.Name, prev.Digest), prev); writeErr != nil {
				return writeErr
			}
		}
	}

	tx.Put(tagKey(scope, r.Name, tag), []byte(digest))
	return nil
}

func (s *Service) PutImage(ctx context.Context, in *api.PutImageRequest) (*api.PutImageResponse, error) {
	if in.ImageManifest == nil {
		return nil, invalidParameter("imageManifest is required.")
	}

	var out *api.Image
	err := s.update(ctx, in.RegistryId, in.RepositoryName,
		func(tx storage.WriteTx, m *request.Metadata, r *repository) error {
			img, err := s.putImage(tx, m.Scope, r, &push{
				Manifest: []byte(*in.ImageManifest), MediaType: value(in.ImageManifestMediaType),
				Tag: value(in.ImageTag), Digest: value(in.ImageDigest),
			})
			if err != nil {
				return err
			}

			out = &api.Image{
				RegistryId: new(m.AccountID), RepositoryName: new(r.Name),
				ImageId:       &api.ImageIdentifier{ImageDigest: new(img.Digest), ImageTag: in.ImageTag},
				ImageManifest: new(img.Manifest), ImageManifestMediaType: new(img.MediaType),
			}
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.PutImageResponse{Image: out}, nil
}

// imageFailure is why an image ID names no image.
type imageFailure struct{ code, reason string }

func (f *imageFailure) output(id api.ImageIdentifier) api.ImageFailure {
	return api.ImageFailure{ImageId: &id, FailureCode: new(f.code), FailureReason: new(f.reason)}
}

// findImage returns the image id names in r, or why there is none.
func findImage(tx storage.ReadTx, scope request.Scope, r *repository, id api.ImageIdentifier) (
	*image, *imageFailure, error,
) {
	digest, tag := value(id.ImageDigest), value(id.ImageTag)
	switch {
	case digest == "" && tag == "":
		return nil, &imageFailure{"MissingDigestAndTag", "Invalid request parameters: both tag and digest cannot be " +
			"null"}, nil
	case digest != "" && !digestPattern.MatchString(digest):
		return nil, &imageFailure{"InvalidImageDigest", "Invalid request parameters: image digest should satisfy the " +
			"regex '[a-zA-Z0-9-_+.]+:[a-fA-F0-9]+'"}, nil
	case tag != "" && !tagPattern.MatchString(tag):
		return nil, &imageFailure{"InvalidImageTag", "Invalid request parameters: invalid image tag"}, nil
	}

	if tag != "" {
		d, ok := tx.Get(tagKey(scope, r.Name, tag))
		if !ok {
			return nil, &imageFailure{failureNotFound, "Requested image not found"}, nil
		}

		if digest != "" && digest != string(d) {
			return nil, &imageFailure{"ImageTagDoesNotMatchDigest", "Invalid request parameters: given tag does " +
				"not map to the given digest"}, nil
		}

		digest = string(d)
	}

	img, ok, err := read[image](tx, imageKey(scope, r.Name, digest))
	if err != nil || !ok {
		return nil, &imageFailure{failureNotFound, "Requested image not found"}, err
	}

	return img, nil, nil
}

func (s *Service) BatchGetImage(ctx context.Context, in *api.BatchGetImageRequest) (*api.BatchGetImageResponse, error) {
	if len(in.ImageIds) < 1 || len(in.ImageIds) > 100 {
		return nil, invalidParameter("imageIds must name 1 to 100 images.")
	}

	out := &api.BatchGetImageResponse{Images: []api.Image{}, Failures: []api.ImageFailure{}}
	err := s.update(ctx, in.RegistryId, in.RepositoryName,
		func(tx storage.WriteTx, m *request.Metadata, r *repository) error {
			for _, id := range in.ImageIds {
				img, failure, err := findImage(tx, m.Scope, r, id)
				if err != nil {
					return err
				}

				if failure == nil && len(in.AcceptedMediaTypes) > 0 &&
					!slices.Contains(in.AcceptedMediaTypes, img.MediaType) {
					failure = &imageFailure{failureNotFound, "The image's manifest media type " + img.MediaType +
						" is not among acceptedMediaTypes"}
				}

				if failure != nil {
					out.Failures = append(out.Failures, failure.output(id))
					continue
				}

				if pullErr := s.pulled(tx, m.Scope, r, img); pullErr != nil {
					return pullErr
				}

				out.Images = append(out.Images, api.Image{
					RegistryId: new(m.AccountID), RepositoryName: new(r.Name),
					ImageId:       &api.ImageIdentifier{ImageDigest: new(img.Digest), ImageTag: id.ImageTag},
					ImageManifest: new(img.Manifest), ImageManifestMediaType: new(img.MediaType),
				})
			}

			return nil
		})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// pulled records that img was pulled now.
func (s *Service) pulled(tx storage.WriteTx, scope request.Scope, r *repository, img *image) error {
	img.Pulled = s.now().UTC()
	return write(tx, imageKey(scope, r.Name, img.Digest), img)
}

// images returns r's images in digest order.
func images(tx storage.ReadTx, scope request.Scope, r *repository) ([]*image, error) {
	var out []*image
	for _, k := range tx.Keys(owned(scope, "image", r.Name)) {
		img, _, err := read[image](tx, k)
		if err != nil {
			return nil, err
		}

		out = append(out, img)
	}

	return out, nil
}

func checkTagStatus(status *string) error {
	if !oneOf(status, tagged, untagged, anyTag) {
		return invalidParameter("tagStatus must be TAGGED, UNTAGGED, or ANY.")
	}

	return nil
}

// selected reports whether img passes a tagStatus filter.
func selected(img *image, status *string) bool {
	switch value(status) {
	case tagged:
		return len(img.Tags) > 0
	case untagged:
		return len(img.Tags) == 0
	default:
		return true
	}
}

func oneOf(v *string, allowed ...string) bool {
	return v == nil || slices.Contains(allowed, *v)
}

func (s *Service) ListImages(ctx context.Context, in *api.ListImagesRequest) (*api.ListImagesResponse, error) {
	status := value(in.Filter).TagStatus
	if err := checkTagStatus(status); err != nil {
		return nil, err
	}

	n, err := limit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	out := &api.ListImagesResponse{ImageIds: []api.ImageIdentifier{}}
	err = s.view(ctx, in.RegistryId, in.RepositoryName,
		func(tx storage.ReadTx, m *request.Metadata, r *repository) error {
			all, imagesErr := images(tx, m.Scope, r)
			if imagesErr != nil {
				return imagesErr
			}

			var ids []api.ImageIdentifier
			for _, img := range all {
				if !selected(img, status) {
					continue
				}

				if len(img.Tags) == 0 {
					ids = append(ids, api.ImageIdentifier{ImageDigest: new(img.Digest)})
				}

				for _, t := range img.Tags {
					ids = append(ids, api.ImageIdentifier{ImageDigest: new(img.Digest), ImageTag: new(t)})
				}
			}

			key := func(id api.ImageIdentifier) string { return *id.ImageDigest + "/" + value(id.ImageTag) }
			ids, out.NextToken, err = page(ids, key, in.NextToken, n)
			out.ImageIds = append(out.ImageIds, ids...)
			return err
		})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (img *image) detail(m *request.Metadata, r *repository) api.ImageDetail {
	d := api.ImageDetail{
		RegistryId: new(m.AccountID), RepositoryName: new(r.Name), ImageDigest: new(img.Digest),
		ImageTags: img.Tags, ImageSizeInBytes: new(img.Size), ImagePushedAt: seconds(img.Pushed),
		ImageManifestMediaType: new(img.MediaType),
	}
	if img.ArtifactMediaType != "" {
		d.ArtifactMediaType = new(img.ArtifactMediaType)
	}

	if !img.Pulled.IsZero() {
		d.LastRecordedPullTime = seconds(img.Pulled)
	}

	if img.Subject != "" {
		d.SubjectManifestDigest = new(img.Subject)
	}

	return d
}

func (s *Service) DescribeImages(
	ctx context.Context,
	in *api.DescribeImagesRequest,
) (*api.DescribeImagesResponse, error) {
	status := value(in.Filter).TagStatus
	if err := checkTagStatus(status); err != nil {
		return nil, err
	}

	if len(in.ImageIds) > 0 && (in.MaxResults != nil || in.NextToken != nil) {
		return nil, invalidParameter("maxResults and nextToken cannot be used with imageIds.")
	}

	n, err := limit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeImagesResponse{ImageDetails: []api.ImageDetail{}}
	err = s.view(ctx, in.RegistryId, in.RepositoryName,
		func(tx storage.ReadTx, m *request.Metadata, r *repository) error {
			var found []*image
			for _, id := range in.ImageIds {
				img, failure, findErr := findImage(tx, m.Scope, r, id)
				if findErr != nil {
					return findErr
				}

				if failure != nil {
					return awserror.New("ImageNotFoundException", fmt.Sprintf("The image with imageId {imageDigest:"+
						"'%s', imageTag:'%s'} does not exist within the repository with name '%s' in the registry "+
						"with id '%s'", value(id.ImageDigest), value(id.ImageTag), r.Name, m.AccountID))
				}

				found = append(found, img)
			}

			if len(in.ImageIds) == 0 {
				all, imagesErr := images(tx, m.Scope, r)
				if imagesErr != nil {
					return imagesErr
				}

				all = slices.DeleteFunc(all, func(img *image) bool { return !selected(img, status) })
				if found, out.NextToken, err = page(all, func(img *image) string { return img.Digest },
					in.NextToken, n); err != nil {
					return err
				}
			}

			for _, img := range found {
				out.ImageDetails = append(out.ImageDetails, img.detail(m, r))
			}

			return nil
		})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// referencedBy returns the digest of an index in r that lists digest, if any.
func referencedBy(tx storage.ReadTx, scope request.Scope, r *repository, digest string) (string, error) {
	all, err := images(tx, scope, r)
	if err != nil {
		return "", err
	}

	for _, img := range all {
		if slices.Contains(img.References, digest) {
			return img.Digest, nil
		}
	}

	return "", nil
}

// deleteImage removes img and its tags from r, unless an index lists it, in
// which case it returns that index's digest.
func deleteImage(tx storage.WriteTx, scope request.Scope, r *repository, img *image) (index string, err error) {
	if index, err = referencedBy(tx, scope, r, img.Digest); err != nil || index != "" {
		return index, err
	}

	for _, t := range img.Tags {
		tx.Delete(tagKey(scope, r.Name, t))
	}

	tx.Delete(imageKey(scope, r.Name, img.Digest))
	return "", nil
}

// BatchDeleteImage deletes images by digest, or removes tags; removing an
// image's last tag deletes the image.
func (s *Service) BatchDeleteImage(
	ctx context.Context,
	in *api.BatchDeleteImageRequest,
) (*api.BatchDeleteImageResponse, error) {
	if len(in.ImageIds) < 1 || len(in.ImageIds) > 100 {
		return nil, invalidParameter("imageIds must name 1 to 100 images.")
	}

	out := &api.BatchDeleteImageResponse{ImageIds: []api.ImageIdentifier{}, Failures: []api.ImageFailure{}}
	err := s.update(ctx, in.RegistryId, in.RepositoryName,
		func(tx storage.WriteTx, m *request.Metadata, r *repository) error {
			for _, id := range in.ImageIds {
				deleted, failure, err := untag(tx, m.Scope, r, id)
				if err != nil {
					return err
				}

				if failure != nil {
					out.Failures = append(out.Failures, failure.output(id))
					continue
				}

				out.ImageIds = append(out.ImageIds, deleted...)
			}

			return nil
		})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// untag deletes the image id names, or only its tag when id names just a tag
// and the image has others, and returns the image IDs removed.
func untag(tx storage.WriteTx, scope request.Scope, r *repository, id api.ImageIdentifier) (
	[]api.ImageIdentifier, *imageFailure, error,
) {
	img, failure, err := findImage(tx, scope, r, id)
	if err != nil || failure != nil {
		return nil, failure, err
	}

	if id.ImageDigest == nil && len(img.Tags) > 1 {
		img.Tags = slices.DeleteFunc(img.Tags, func(t string) bool { return t == *id.ImageTag })
		tx.Delete(tagKey(scope, r.Name, *id.ImageTag))
		return []api.ImageIdentifier{{ImageDigest: new(img.Digest), ImageTag: id.ImageTag}},
			nil, write(tx, imageKey(scope, r.Name, img.Digest), img)
	}

	index, err := deleteImage(tx, scope, r, img)
	if err != nil {
		return nil, nil, err
	}

	if index != "" {
		return nil, &imageFailure{"ImageReferencedByManifestList", "Requested image referenced by manifest list: [" +
			index + "]"}, nil
	}

	if len(img.Tags) == 0 {
		return []api.ImageIdentifier{{ImageDigest: new(img.Digest)}}, nil, nil
	}

	out := make([]api.ImageIdentifier, 0, len(img.Tags))
	for _, t := range img.Tags {
		out = append(out, api.ImageIdentifier{ImageDigest: new(img.Digest), ImageTag: new(t)})
	}

	return out, nil, nil
}
