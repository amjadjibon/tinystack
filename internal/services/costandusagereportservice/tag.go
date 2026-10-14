package costandusagereportservice

import (
	"context"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/costandusagereportservice"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func checkTags(tags []api.Tag) (map[string]string, error) {
	if len(tags) > 200 {
		return nil, awserror.Validation("Tags must contain at most 200 entries")
	}

	out, err := tagging.Map(tags, func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		awserror.Validation("Tag Key and Value are required"), nil)
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(out); err != nil {
		return nil, awserror.Validation("%s", err)
	}

	return out, nil
}

// changeTags reads the named report and applies a tag mutation in one transaction.
func (s *Service) changeTags(ctx context.Context, name *string, change func(*reportRecord) error) error {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return err
	}

	if checkErr := checkString("ReportName", name, namePattern); checkErr != nil {
		return checkErr
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, "report", *name)
		r, exists, e := storage.Read[reportRecord](tx, key)
		if e != nil {
			return e
		}

		if !exists {
			return awserror.New("ResourceNotFoundException", "Report does not exist")
		}

		if e := change(r); e != nil {
			return e
		}

		return storage.Write(tx, key, r)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	if in.Tags == nil {
		return nil, awserror.Validation("Tags is required")
	}

	tags, err := checkTags(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.changeTags(ctx, in.ReportName, func(r *reportRecord) error {
		merged, e := tagging.Merge(r.Tags, tags)
		if e != nil {
			return awserror.Validation("%s", e)
		}

		r.Tags = merged
		return nil
	})
	return &api.TagResourceResponse{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	if len(in.TagKeys) > 200 {
		return nil, awserror.Validation("TagKeys must contain at most 200 entries")
	}

	if in.TagKeys == nil {
		return nil, awserror.Validation("TagKeys is required")
	}

	for _, k := range in.TagKeys {
		if n := utf8.RuneCountInString(k); n < 1 || n > 128 {
			return nil, awserror.Validation("Tag keys must be 1 to 128 characters")
		}
	}

	err := s.changeTags(ctx, in.ReportName, func(r *reportRecord) error {
		r.Tags = tagging.Remove(r.Tags, in.TagKeys)
		return nil
	})
	return &api.UntagResourceResponse{}, err
}

func (s *Service) ListTagsForResource(ctx context.Context, in *api.ListTagsForResourceRequest) (
	*api.ListTagsForResourceResponse, error,
) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	if checkErr := checkString("ReportName", in.ReportName, namePattern); checkErr != nil {
		return nil, checkErr
	}

	out := &api.ListTagsForResourceResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, exists, e := storage.Read[reportRecord](tx, scope.Key(serviceName, "report", *in.ReportName))
		if e != nil {
			return e
		}

		if !exists {
			return awserror.New("ResourceNotFoundException", "Report does not exist")
		}

		out.Tags = tagging.List(r.Tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
		return nil
	})
	return out, err
}
