// Package polly emulates local speech responses, lexicons, and S3 synthesis tasks.
package polly

import (
	"context"
	"net/http"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/polly"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/mlfixtures"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName = "polly"
	synthesize  = "SynthesizeSpeech"
	standard    = "standard"
)

type Service struct {
	store   storage.Store
	buckets *s3.Service
	workers *background.Group
}

func New(store storage.Store, buckets *s3.Service) *Service {
	return &Service{store: store, buckets: buckets, workers: background.New()}
}

func invalidToken() error {
	return awserror.New("InvalidNextTokenException", "invalid NextToken")
}

type voice struct{ id, gender, language, languageName string }

var voices = []voice{
	{id: "Amy", gender: "Female", language: "en-GB", languageName: "British English"},
	{id: "Joanna", gender: "Female", language: "en-US", languageName: "US English"},
	{id: "Matthew", gender: "Male", language: "en-US", languageName: "US English"},
}

func (s *Service) DescribeVoices(_ context.Context, in *api.DescribeVoicesInput) (*api.DescribeVoicesOutput, error) {
	var items []api.Voice
	if *common.OrElse(in.Engine, new(standard)) == standard {
		for _, v := range voices {
			if in.LanguageCode != nil && *in.LanguageCode != v.language {
				continue
			}

			items = append(items, api.Voice{
				Id: new(v.id), Gender: new(v.gender), Name: new(v.id), LanguageCode: new(v.language),
				LanguageName: new(v.languageName), SupportedEngines: []string{standard},
			})
		}
	}

	page, next, err := common.Page(
		items, func(v api.Voice) string { return common.Value(v.Id) }, in.NextToken, 100, invalidToken(),
	)
	return &api.DescribeVoicesOutput{Voices: page, NextToken: next}, err
}

func Handler(store storage.Store, account string) http.Handler {
	return mlfixtures.Handler(store, account, serviceName, validateFixture)
}

var _ api.API = (*Service)(nil)
