// Package comprehend returns fixture-backed text analysis.
package comprehend

import (
	"context"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/comprehend"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/mlfixtures"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const serviceName = "comprehend"

const (
	sentiment   = "DetectSentiment"
	entities    = "DetectEntities"
	keyPhrases  = "DetectKeyPhrases"
	language    = "DetectDominantLanguage"
	pii         = "DetectPiiEntities"
	syntax      = "DetectSyntax"
	containsPII = "ContainsPiiEntities"
)

type Service struct{ store storage.Store }

func New(store storage.Store) *Service { return &Service{store: store} }

type input struct {
	Text         string `json:"text"`
	LanguageCode string `json:"languageCode,omitempty"`
}

func validate(op string, in input) error {
	if in.Text == "" || !utf8.ValidString(in.Text) {
		return awserror.New("InvalidRequestException", "Text must be nonempty UTF-8")
	}

	maxBytes := 5000
	switch op {
	case entities, keyPhrases, language, pii, containsPII:
		maxBytes = 100000
	}

	if len(in.Text) > maxBytes {
		return awserror.New("TextSizeLimitExceededException", "Text exceeds the operation's UTF-8 byte limit")
	}

	if op == language {
		if utf8.RuneCountInString(in.Text) < 20 {
			return awserror.New("InvalidRequestException", "DetectDominantLanguage requires at least 20 characters")
		}

		if in.LanguageCode != "" {
			return awserror.New("InvalidRequestException", "DetectDominantLanguage takes no input language")
		}

		return nil
	}

	switch in.LanguageCode {
	case "en", "es":
		return nil
	case "de", "fr", "it", "pt":
		if op != pii && op != containsPII {
			return nil
		}
	case "ar", "hi", "ja", "ko", "zh", "zh-TW":
		if op != syntax && op != pii && op != containsPII {
			return nil
		}
	}

	return awserror.New("UnsupportedLanguageException", "LanguageCode is unsupported for this operation")
}

func Handler(store storage.Store, account string) http.Handler {
	return mlfixtures.Handler(store, account, serviceName, func(f *mlfixtures.Reply[input]) error {
		if err := validate(f.Operation, f.Input); err != nil {
			return err
		}

		switch f.Operation {
		case sentiment:
			return mlfixtures.ValidateOutput[api.DetectSentimentResponse](f.Output)
		case entities:
			return mlfixtures.ValidateOutput[api.DetectEntitiesResponse](f.Output)
		case keyPhrases:
			return mlfixtures.ValidateOutput[api.DetectKeyPhrasesResponse](f.Output)
		case language:
			return mlfixtures.ValidateOutput[api.DetectDominantLanguageResponse](f.Output)
		case pii:
			return mlfixtures.ValidateOutput[api.DetectPiiEntitiesResponse](f.Output)
		case syntax:
			return mlfixtures.ValidateOutput[api.DetectSyntaxResponse](f.Output)
		case containsPII:
			return mlfixtures.ValidateOutput[api.ContainsPiiEntitiesResponse](f.Output)
		default:
			return errors.New("unsupported Comprehend fixture operation")
		}
	})
}

func analyze[T any](ctx context.Context, s *Service, op string, text, code *string) (*T, error) {
	in := input{Text: common.Value(text), LanguageCode: common.Value(code)}
	if err := validate(op, in); err != nil {
		return nil, err
	}

	raw, err := mlfixtures.Find(ctx, s.store, serviceName, op, func(key input) bool { return key == in })
	if errors.Is(err, mlfixtures.ErrAmbiguous) || (err == nil && raw == nil) {
		return nil, awserror.New(
			"InvalidRequestException",
			"no unique TinyStack Comprehend fixture; register one at /_tinystack/comprehend/fixtures",
		)
	}

	if err != nil {
		return nil, err
	}

	return mlfixtures.Decode[T](raw)
}

func (s *Service) DetectSentiment(ctx context.Context, in *api.DetectSentimentRequest) (
	*api.DetectSentimentResponse, error,
) {
	return analyze[api.DetectSentimentResponse](ctx, s, sentiment, in.Text, in.LanguageCode)
}

func (s *Service) DetectEntities(ctx context.Context, in *api.DetectEntitiesRequest) (
	*api.DetectEntitiesResponse, error,
) {
	return analyze[api.DetectEntitiesResponse](ctx, s, entities, in.Text, in.LanguageCode)
}

func (s *Service) DetectKeyPhrases(ctx context.Context, in *api.DetectKeyPhrasesRequest) (
	*api.DetectKeyPhrasesResponse, error,
) {
	return analyze[api.DetectKeyPhrasesResponse](ctx, s, keyPhrases, in.Text, in.LanguageCode)
}

func (s *Service) DetectDominantLanguage(ctx context.Context, in *api.DetectDominantLanguageRequest) (
	*api.DetectDominantLanguageResponse, error,
) {
	return analyze[api.DetectDominantLanguageResponse](ctx, s, language, in.Text, nil)
}

func (s *Service) DetectPiiEntities(ctx context.Context, in *api.DetectPiiEntitiesRequest) (
	*api.DetectPiiEntitiesResponse, error,
) {
	return analyze[api.DetectPiiEntitiesResponse](ctx, s, pii, in.Text, in.LanguageCode)
}

func (s *Service) DetectSyntax(ctx context.Context, in *api.DetectSyntaxRequest) (*api.DetectSyntaxResponse, error) {
	return analyze[api.DetectSyntaxResponse](ctx, s, syntax, in.Text, in.LanguageCode)
}

func (s *Service) ContainsPiiEntities(ctx context.Context, in *api.ContainsPiiEntitiesRequest) (
	*api.ContainsPiiEntitiesResponse, error,
) {
	return analyze[api.ContainsPiiEntitiesResponse](ctx, s, containsPII, in.Text, in.LanguageCode)
}

var _ api.API = (*Service)(nil)
