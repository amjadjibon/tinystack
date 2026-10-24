// Package translate returns translations exclusively from user-registered fixtures.
package translate

import (
	"context"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	kmsapi "github.com/amjadjibon/tinystack/internal/aws/generated/kms"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/translate"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/services/kms"
	"github.com/amjadjibon/tinystack/internal/services/mlfixtures"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const serviceName = "translate"

// Service owns regional terminology, parallel data, and batch job records.
type Service struct {
	store   storage.Store
	buckets *s3.Service
	roles   *iam.Service
	keys    *kms.Service
	workers *background.Group
}

func New(store storage.Store, buckets *s3.Service, roles *iam.Service, keys *kms.Service) *Service {
	return &Service{store: store, buckets: buckets, roles: roles, keys: keys, workers: background.New()}
}

func invalid(format string, args ...any) error {
	return awserror.New("InvalidRequestException", fmt.Sprintf(format, args...))
}

func missing(name string) error {
	return awserror.New("ResourceNotFoundException", "Resource does not exist: "+name)
}

func now() float64 { return float64(time.Now().UnixMilli()) / 1000 }

func arn(scope request.Scope, kind, name string) string {
	if kind == kindTerminology {
		name += "/LATEST"
	}

	return fmt.Sprintf("arn:aws:translate:%s:%s:%s/%s", scope.Region, scope.AccountID, kind, name)
}

func limit(v *int64) int {
	if v == nil {
		return 100
	}

	return int(*v)
}

func read[T any](tx storage.ReadTx, scope request.Scope, kind, name string) (*T, error) {
	r, ok, err := storage.Read[T](tx, scope.Key(serviceName, kind, name))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing(name)
	}

	return r, nil
}

func (s *Service) encryption(ctx context.Context, key *api.EncryptionKey) error {
	if key == nil {
		return nil
	}

	_, err := s.keys.DescribeKey(ctx, &kmsapi.DescribeKeyRequest{KeyId: key.Id})
	if err != nil {
		return invalid("EncryptionKey must name an existing TinyStack KMS key: %s", err)
	}

	return nil
}

// Fixture matches all three inputs exactly. Auto detection requires one candidate.
type Fixture struct {
	TranslatedText     *string `json:"translatedText"`
	SourceLanguageCode string  `json:"sourceLanguageCode"`
	TargetLanguageCode string  `json:"targetLanguageCode"`
	Text               string  `json:"text"`
}

func Handler(store storage.Store, account string) http.Handler {
	return mlfixtures.Handler(store, account, serviceName, func(f *Fixture) error {
		if f.TranslatedText == nil || f.SourceLanguageCode == "" || f.SourceLanguageCode == "auto" ||
			f.TargetLanguageCode == "" ||
			f.Text == "" {
			return fmt.Errorf("sourceLanguageCode (not auto), targetLanguageCode, text and translatedText are required")
		}

		return nil
	})
}

func (s *Service) match(ctx context.Context, source, target, text string) (*Fixture, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	scope.Region = ""
	var matches []*Fixture
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rows, e := storage.Records[Fixture](tx, scope.Prefix(serviceName, "fixture"))
		if e != nil {
			return e
		}

		for _, r := range rows {
			if r.TargetLanguageCode == target && r.Text == text &&
				(source == "auto" || r.SourceLanguageCode == source) {
				matches = append(matches, r)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(matches) != 1 {
		return nil, invalid(
			"no unique TinyStack translation fixture for %s->%s; register one at /_tinystack/translate/fixtures",
			source,
			target,
		)
	}

	return matches[0], nil
}

func (s *Service) options(ctx context.Context, names []string, settings *api.TranslationSettings) error {
	if len(names) > 1 {
		return invalid("at most one terminology may be supplied")
	}

	if settings != nil && (settings.Formality != nil || settings.Profanity != nil || settings.Brevity != nil) {
		return invalid("translation Settings are not emulated; fixtures are returned verbatim")
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, name := range names {
			if _, e := read[terminologyRecord](tx, scope, kindTerminology, name); e != nil {
				return e
			}
		}

		return nil
	})
}

func (s *Service) TranslateText(ctx context.Context, in *api.TranslateTextRequest) (*api.TranslateTextResponse, error) {
	if !utf8.ValidString(common.Value(in.Text)) {
		return nil, invalid("Text must be UTF-8")
	}

	if err := s.options(ctx, in.TerminologyNames, in.Settings); err != nil {
		return nil, err
	}

	f, err := s.match(
		ctx,
		common.Value(in.SourceLanguageCode),
		common.Value(in.TargetLanguageCode),
		common.Value(in.Text),
	)
	if err != nil {
		return nil, err
	}

	return &api.TranslateTextResponse{
		TranslatedText:       f.TranslatedText,
		SourceLanguageCode:   new(f.SourceLanguageCode),
		TargetLanguageCode:   new(f.TargetLanguageCode),
		AppliedTerminologies: []api.AppliedTerminology{},
	}, nil
}

func (s *Service) TranslateDocument(
	ctx context.Context,
	in *api.TranslateDocumentRequest,
) (*api.TranslateDocumentResponse, error) {
	if err := contentType(common.Value(in.Document.ContentType)); err != nil {
		return nil, err
	}

	out, err := s.TranslateText(
		ctx,
		&api.TranslateTextRequest{
			Text:               new(string(in.Document.Content)),
			SourceLanguageCode: in.SourceLanguageCode,
			TargetLanguageCode: in.TargetLanguageCode,
			TerminologyNames:   in.TerminologyNames,
			Settings:           in.Settings,
		},
	)
	if err != nil {
		return nil, err
	}

	if common.Value(out.SourceLanguageCode) != "en" && common.Value(out.TargetLanguageCode) != "en" {
		return nil, invalid("real-time document translation requires English as source or target")
	}

	return &api.TranslateDocumentResponse{
		TranslatedDocument:   &api.TranslatedDocument{Content: []byte(common.Value(out.TranslatedText))},
		SourceLanguageCode:   out.SourceLanguageCode,
		TargetLanguageCode:   out.TargetLanguageCode,
		AppliedTerminologies: out.AppliedTerminologies,
	}, nil
}

func contentType(v string) error {
	if v != "text/plain" && v != "text/html" {
		return invalid("only text/plain and text/html are supported")
	}

	return nil
}

var (
	_ api.API           = (*Service)(nil)
	_ catalog.Inventory = (*Service)(nil)
)
