// Package rekognition returns fixture-backed image analysis and stores regional collections.
package rekognition

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rekognition"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/mlfixtures"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName = "rekognition"
	labels      = "DetectLabels"
	faces       = "DetectFaces"
	text        = "DetectText"
	moderation  = "DetectModerationLabels"
	compare     = "CompareFaces"
)

type Service struct {
	store   storage.Store
	buckets *s3.Service
}

func New(store storage.Store, buckets *s3.Service) *Service {
	return &Service{store: store, buckets: buckets}
}

type input struct {
	SHA256       string `json:"sha256"`
	TargetSHA256 string `json:"targetSha256,omitempty"`
}

func Handler(store storage.Store, account string) http.Handler {
	return mlfixtures.Handler(store, account, serviceName, func(f *mlfixtures.Reply[input]) error {
		if !mlfixtures.ValidDigest(f.Input.SHA256) {
			return errors.New("input.sha256 must be a lowercase SHA-256 hex digest")
		}

		if f.Operation == compare {
			if !mlfixtures.ValidDigest(f.Input.TargetSHA256) {
				return errors.New("CompareFaces requires input.targetSha256 as a lowercase SHA-256 hex digest")
			}
		} else if f.Input.TargetSHA256 != "" {
			return errors.New("input.targetSha256 is only supported for CompareFaces")
		}

		switch f.Operation {
		case labels:
			return mlfixtures.ValidateOutput[api.DetectLabelsResponse](f.Output)
		case faces:
			return mlfixtures.ValidateOutput[api.DetectFacesResponse](f.Output)
		case text:
			return mlfixtures.ValidateOutput[api.DetectTextResponse](f.Output)
		case moderation:
			return mlfixtures.ValidateOutput[api.DetectModerationLabelsResponse](f.Output)
		case compare:
			return mlfixtures.ValidateOutput[api.CompareFacesResponse](f.Output)
		default:
			return errors.New("unsupported Rekognition fixture operation")
		}
	})
}

func (s *Service) digest(ctx context.Context, image *api.Image) (string, error) {
	var object *s3api.GetObjectRequest
	limit := 5 << 20
	if image.S3Object != nil {
		ref := image.S3Object
		object = &s3api.GetObjectRequest{Bucket: ref.Bucket, Key: ref.Name, VersionId: ref.Version}
		limit = 15 << 20
	}

	return mlfixtures.ImageDigest(
		ctx, s.buckets, image.Bytes, object, limit, "ImageTooLargeException", "InvalidImageFormatException",
	)
}

func detect[T any](ctx context.Context, s *Service, op string, source, target *api.Image) (*T, error) {
	digest, err := s.digest(ctx, source)
	if err != nil {
		return nil, err
	}

	in := input{SHA256: digest}
	if target != nil {
		in.TargetSHA256, err = s.digest(ctx, target)
		if err != nil {
			return nil, err
		}
	}

	raw, err := mlfixtures.Find(ctx, s.store, serviceName, op, func(key input) bool { return key == in })
	if errors.Is(err, mlfixtures.ErrAmbiguous) || (err == nil && raw == nil) {
		return nil, invalid("no unique TinyStack Rekognition fixture; register one at /_tinystack/rekognition/fixtures")
	}

	if err != nil {
		return nil, err
	}

	return mlfixtures.Decode[T](raw)
}

func (s *Service) DetectLabels(ctx context.Context, in *api.DetectLabelsRequest) (*api.DetectLabelsResponse, error) {
	out, err := detect[api.DetectLabelsResponse](ctx, s, labels, in.Image, nil)
	if err != nil {
		return nil, err
	}

	minimum := *common.OrElse(in.MinConfidence, new(float64(55)))
	out.Labels = slices.DeleteFunc(out.Labels, func(label api.Label) bool {
		return common.Value(label.Confidence) < minimum
	})
	if in.MaxLabels != nil && *in.MaxLabels < int64(len(out.Labels)) {
		slices.SortStableFunc(out.Labels, func(a, b api.Label) int {
			return cmp.Compare(common.Value(b.Confidence), common.Value(a.Confidence))
		})
		out.Labels = out.Labels[:*in.MaxLabels]
	}

	return out, nil
}

func (s *Service) DetectFaces(ctx context.Context, in *api.DetectFacesRequest) (*api.DetectFacesResponse, error) {
	return detect[api.DetectFacesResponse](ctx, s, faces, in.Image, nil)
}

func (s *Service) DetectText(ctx context.Context, in *api.DetectTextRequest) (*api.DetectTextResponse, error) {
	return detect[api.DetectTextResponse](ctx, s, text, in.Image, nil)
}

func (s *Service) DetectModerationLabels(ctx context.Context, in *api.DetectModerationLabelsRequest) (
	*api.DetectModerationLabelsResponse, error,
) {
	out, err := detect[api.DetectModerationLabelsResponse](ctx, s, moderation, in.Image, nil)
	if err != nil {
		return nil, err
	}

	minimum := *common.OrElse(in.MinConfidence, new(float64(50)))
	out.ModerationLabels = slices.DeleteFunc(out.ModerationLabels, func(label api.ModerationLabel) bool {
		return common.Value(label.Confidence) < minimum
	})
	return out, nil
}

func (s *Service) CompareFaces(ctx context.Context, in *api.CompareFacesRequest) (*api.CompareFacesResponse, error) {
	out, err := detect[api.CompareFacesResponse](ctx, s, compare, in.SourceImage, in.TargetImage)
	if err != nil {
		return nil, err
	}

	minimum := *common.OrElse(in.SimilarityThreshold, new(float64(80)))
	out.FaceMatches = slices.DeleteFunc(out.FaceMatches, func(match api.CompareFacesMatch) bool {
		if common.Value(match.Similarity) >= minimum {
			return false
		}

		if match.Face != nil {
			out.UnmatchedFaces = append(out.UnmatchedFaces, *match.Face)
		}

		return true
	})
	return out, nil
}

func invalid(message string) error { return awserror.New("InvalidParameterException", message) }

var _ api.API = (*Service)(nil)
