// Package textract returns fixture-backed document analysis.
package textract

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/textract"
	"github.com/amjadjibon/tinystack/internal/services/mlfixtures"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName = "textract"
	detect      = "DetectDocumentText"
	analyze     = "AnalyzeDocument"
)

type Service struct {
	store   storage.Store
	buckets *s3.Service
}

func New(store storage.Store, buckets *s3.Service) *Service {
	return &Service{store: store, buckets: buckets}
}

type input struct {
	SHA256       string   `json:"sha256"`
	FeatureTypes []string `json:"featureTypes,omitempty"`
}

func features(values []string) ([]string, error) {
	list := slices.Clone(values)
	slices.Sort(list)
	if len(list) == 0 || len(list) != len(slices.Compact(list)) {
		return nil, awserror.New("InvalidParameterException", "FeatureTypes must be nonempty and unique")
	}

	for _, feature := range list {
		switch feature {
		case "FORMS", "LAYOUT", "SIGNATURES", "TABLES":
		default:
			return nil, awserror.New("InvalidParameterException", "unsupported FeatureType: "+feature)
		}
	}

	return list, nil
}

func Handler(store storage.Store, account string) http.Handler {
	return mlfixtures.Handler(store, account, serviceName, func(f *mlfixtures.Reply[input]) error {
		if !mlfixtures.ValidDigest(f.Input.SHA256) {
			return errors.New("input.sha256 must be a lowercase SHA-256 hex digest")
		}

		switch f.Operation {
		case detect:
			if len(f.Input.FeatureTypes) != 0 {
				return errors.New("DetectDocumentText takes no feature types")
			}

			return mlfixtures.ValidateOutput[api.DetectDocumentTextResponse](f.Output)
		case analyze:
			list, err := features(f.Input.FeatureTypes)
			if err != nil {
				return err
			}

			f.Input.FeatureTypes = list
			return mlfixtures.ValidateOutput[api.AnalyzeDocumentResponse](f.Output)
		default:
			return errors.New("unsupported Textract fixture operation")
		}
	})
}

func scan[T any](ctx context.Context, s *Service, op string, doc *api.Document, types []string) (*T, error) {
	var object *s3api.GetObjectRequest
	if doc.S3Object != nil {
		ref := doc.S3Object
		object = &s3api.GetObjectRequest{Bucket: ref.Bucket, Key: ref.Name, VersionId: ref.Version}
	}

	digest, err := mlfixtures.ImageDigest(
		ctx, s.buckets, doc.Bytes, object, 10<<20, "DocumentTooLargeException", "UnsupportedDocumentException",
	)
	if err != nil {
		return nil, err
	}

	raw, err := mlfixtures.Find(ctx, s.store, serviceName, op, func(key input) bool {
		return key.SHA256 == digest && slices.Equal(types, key.FeatureTypes)
	})
	if errors.Is(err, mlfixtures.ErrAmbiguous) || (err == nil && raw == nil) {
		return nil, awserror.New(
			"InvalidParameterException",
			"no unique TinyStack Textract fixture; register one at /_tinystack/textract/fixtures",
		)
	}

	if err != nil {
		return nil, err
	}

	return mlfixtures.Decode[T](raw)
}

func (s *Service) DetectDocumentText(ctx context.Context, in *api.DetectDocumentTextRequest) (
	*api.DetectDocumentTextResponse, error,
) {
	return scan[api.DetectDocumentTextResponse](ctx, s, detect, in.Document, nil)
}

func (s *Service) AnalyzeDocument(ctx context.Context, in *api.AnalyzeDocumentRequest) (
	*api.AnalyzeDocumentResponse, error,
) {
	list, err := features(in.FeatureTypes)
	if err != nil {
		return nil, err
	}

	return scan[api.AnalyzeDocumentResponse](ctx, s, analyze, in.Document, list)
}

var _ api.API = (*Service)(nil)
