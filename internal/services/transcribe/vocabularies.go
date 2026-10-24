package transcribe

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/transcribe"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Vocabulary contents are validated/stored, but never alter fixture transcripts.
func (s *Service) saveVocabulary(
	ctx context.Context,
	kind, name, language, uri string,
	words []string,
	tags []api.Tag,
	update bool,
) (*record, error) {
	if (len(words) == 0) == (uri == "") {
		return nil, invalid("exactly one of words/phrases and an S3 file URI is required")
	}

	if uri != "" {
		if err := s.file(ctx, uri); err != nil {
			return nil, err
		}
	}

	tagValues, err := tagMap(tags)
	if err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	r := &record{
		Name:     name,
		Language: language,
		URI:      uri,
		Phrases:  words,
		Modified: now(),
		State:    statusPending,
		Tags:     tagValues,
	}

	if kind == kindFilter {
		r.State = statusReady
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, kind, name)
		old, exists, e := storage.Read[record](tx, key)
		if e != nil {
			return e
		}

		if update {
			if !exists {
				return absent(kind, name)
			}

			if old.State != statusReady && old.State != statusFailed {
				return invalid("vocabulary is not ready for update")
			}

			r.Tags = old.Tags
			if language == "" {
				r.Language = old.Language
			}
		} else if exists {
			return conflict("vocabulary already exists")
		}

		if e := storage.Write(tx, key, r); e != nil {
			return e
		}

		if kind == kindVocabulary {
			return queue(tx, scope, kind, name)
		}

		return nil
	})
	return r, err
}

func (s *Service) CreateVocabulary(
	ctx context.Context,
	in *api.CreateVocabularyRequest,
) (*api.CreateVocabularyResponse, error) {
	r, err := s.saveVocabulary(
		ctx,
		kindVocabulary,
		common.Value(in.VocabularyName),
		common.Value(in.LanguageCode),
		common.Value(in.VocabularyFileUri),
		in.Phrases,
		in.Tags,
		false,
	)
	if err != nil {
		return nil, err
	}

	return &api.CreateVocabularyResponse{
		VocabularyName:   new(r.Name),
		LanguageCode:     new(r.Language),
		LastModifiedTime: new(r.Modified),
		VocabularyState:  new(r.State),
	}, nil
}

func (s *Service) UpdateVocabulary(
	ctx context.Context,
	in *api.UpdateVocabularyRequest,
) (*api.UpdateVocabularyResponse, error) {
	r, err := s.saveVocabulary(
		ctx,
		kindVocabulary,
		common.Value(in.VocabularyName),
		common.Value(in.LanguageCode),
		common.Value(in.VocabularyFileUri),
		in.Phrases,
		nil,
		true,
	)
	if err != nil {
		return nil, err
	}

	return &api.UpdateVocabularyResponse{
		VocabularyName:   new(r.Name),
		LanguageCode:     new(r.Language),
		LastModifiedTime: new(r.Modified),
		VocabularyState:  new(r.State),
	}, nil
}

func (s *Service) GetVocabulary(ctx context.Context, in *api.GetVocabularyRequest) (*api.GetVocabularyResponse, error) {
	r, err := s.get(ctx, kindVocabulary, common.Value(in.VocabularyName))
	if err != nil {
		return nil, err
	}

	// DownloadUri is omitted rather than inventing a presigned download URL.
	return &api.GetVocabularyResponse{
		VocabularyName:   new(r.Name),
		LanguageCode:     new(r.Language),
		LastModifiedTime: new(r.Modified),
		VocabularyState:  new(r.State),
	}, nil
}

func (s *Service) DeleteVocabulary(
	ctx context.Context,
	in *api.DeleteVocabularyRequest,
) (*api.DeleteVocabularyOutput, error) {
	return &api.DeleteVocabularyOutput{}, s.remove(ctx, kindVocabulary, common.Value(in.VocabularyName))
}

func (s *Service) ListVocabularies(
	ctx context.Context,
	in *api.ListVocabulariesRequest,
) (*api.ListVocabulariesResponse, error) {
	rows, err := s.list(ctx, kindVocabulary, common.Value(in.NameContains), common.Value(in.StateEquals))
	if err != nil {
		return nil, err
	}

	items := []api.VocabularyInfo{}
	for _, r := range rows {
		items = append(
			items,
			api.VocabularyInfo{
				VocabularyName:   new(r.Name),
				LanguageCode:     new(r.Language),
				LastModifiedTime: new(r.Modified),
				VocabularyState:  new(r.State),
			},
		)
	}

	page, next, err := common.Page(
		items,
		func(v api.VocabularyInfo) string { return common.Value(v.VocabularyName) },
		in.NextToken,
		limit(in.MaxResults),
		invalid("invalid NextToken"),
	)
	return &api.ListVocabulariesResponse{Vocabularies: page, NextToken: next}, err
}

func (s *Service) CreateVocabularyFilter(
	ctx context.Context,
	in *api.CreateVocabularyFilterRequest,
) (*api.CreateVocabularyFilterResponse, error) {
	r, err := s.saveVocabulary(
		ctx,
		kindFilter,
		common.Value(in.VocabularyFilterName),
		common.Value(in.LanguageCode),
		common.Value(in.VocabularyFilterFileUri),
		in.Words,
		in.Tags,
		false,
	)
	if err != nil {
		return nil, err
	}

	return &api.CreateVocabularyFilterResponse{
		VocabularyFilterName: new(r.Name),
		LanguageCode:         new(r.Language),
		LastModifiedTime:     new(r.Modified),
	}, nil
}

func (s *Service) UpdateVocabularyFilter(
	ctx context.Context,
	in *api.UpdateVocabularyFilterRequest,
) (*api.UpdateVocabularyFilterResponse, error) {
	r, err := s.saveVocabulary(
		ctx,
		kindFilter,
		common.Value(in.VocabularyFilterName),
		"",
		common.Value(in.VocabularyFilterFileUri),
		in.Words,
		nil,
		true,
	)
	if err != nil {
		return nil, err
	}

	return &api.UpdateVocabularyFilterResponse{
		VocabularyFilterName: new(r.Name),
		LanguageCode:         new(r.Language),
		LastModifiedTime:     new(r.Modified),
	}, nil
}

func (s *Service) GetVocabularyFilter(
	ctx context.Context,
	in *api.GetVocabularyFilterRequest,
) (*api.GetVocabularyFilterResponse, error) {
	r, err := s.get(ctx, kindFilter, common.Value(in.VocabularyFilterName))
	if err != nil {
		return nil, err
	}

	// DownloadUri is omitted rather than inventing a presigned download URL.
	return &api.GetVocabularyFilterResponse{
		VocabularyFilterName: new(r.Name),
		LanguageCode:         new(r.Language),
		LastModifiedTime:     new(r.Modified),
	}, nil
}

func (s *Service) DeleteVocabularyFilter(
	ctx context.Context,
	in *api.DeleteVocabularyFilterRequest,
) (*api.DeleteVocabularyFilterOutput, error) {
	return &api.DeleteVocabularyFilterOutput{}, s.remove(
		ctx,
		kindFilter,
		common.Value(in.VocabularyFilterName),
	)
}

func (s *Service) ListVocabularyFilters(
	ctx context.Context,
	in *api.ListVocabularyFiltersRequest,
) (*api.ListVocabularyFiltersResponse, error) {
	rows, err := s.list(ctx, kindFilter, common.Value(in.NameContains), "")
	if err != nil {
		return nil, err
	}

	items := []api.VocabularyFilterInfo{}
	for _, r := range rows {
		items = append(
			items,
			api.VocabularyFilterInfo{
				VocabularyFilterName: new(r.Name),
				LanguageCode:         new(r.Language),
				LastModifiedTime:     new(r.Modified),
			},
		)
	}

	page, next, err := common.Page(
		items,
		func(v api.VocabularyFilterInfo) string { return common.Value(v.VocabularyFilterName) },
		in.NextToken,
		limit(in.MaxResults),
		invalid("invalid NextToken"),
	)
	return &api.ListVocabularyFiltersResponse{VocabularyFilters: page, NextToken: next}, err
}
