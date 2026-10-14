package emrserverless

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/emrserverless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func checkTags(tags map[string]string) error {
	if err := tagging.CheckSet(tags); err != nil {
		return awserror.Validation("%s", err)
	}

	return nil
}

func tagTarget(scope request.Scope, value *string) (applicationID, jobID string, err error) {
	parts := strings.SplitN(common.Value(value), ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" || parts[2] != "emr-serverless" {
		return "", "", awserror.Validation("Invalid EMR Serverless resource ARN.")
	}

	path := strings.Split(parts[5], "/")
	if (len(path) != 3 && len(path) != 5) || path[0] != "" || path[1] != "applications" || path[2] == "" {
		return "", "", awserror.Validation("Invalid EMR Serverless resource ARN.")
	}

	if len(path) == 5 && (path[3] != "jobruns" || path[4] == "") {
		return "", "", awserror.Validation("Invalid job run ARN.")
	}

	if parts[3] != scope.Region || parts[4] != scope.AccountID {
		return "", "", missing()
	}

	if len(path) == 5 {
		return path[2], path[4], nil
	}

	return path[2], "", nil
}

func (s *Service) editTags(
	ctx context.Context,
	value *string,
	change func(map[string]string) (map[string]string, error),
) (map[string]string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	app, id, err := tagTarget(scope, value)
	if err != nil {
		return nil, err
	}

	var out map[string]string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, e := readApplication(tx, scope, new(app))
		if e != nil {
			return e
		}

		if id == "" {
			out, e = change(common.CloneTags(a.Tags))
			if e != nil {
				return e
			}

			a.Tags = out
			return storage.Write(tx, appKey(scope, app), a)
		}

		j, e := readJob(tx, scope, new(app), new(id))
		if e != nil {
			return e
		}

		out, e = change(common.CloneTags(j.Tags))
		if e != nil {
			return e
		}

		j.Tags = out
		return storage.Write(tx, jobKey(scope, app, id), j)
	})
	return out, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	tags, err := s.editTags(
		ctx,
		in.ResourceArn,
		func(tags map[string]string) (map[string]string, error) { return tags, nil },
	)
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{Tags: tags}, nil
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	if len(in.Tags) == 0 {
		return nil, awserror.Validation("tags must not be empty.")
	}

	if err := checkTags(in.Tags); err != nil {
		return nil, err
	}

	_, err := s.editTags(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string, error) {
		merged, e := tagging.Merge(tags, in.Tags)
		if e != nil {
			return nil, awserror.Validation("%s", e)
		}

		return merged, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	if len(in.TagKeys) < 1 || len(in.TagKeys) > tagging.Max {
		return nil, awserror.Validation("tagKeys must contain 1 to 50 keys.")
	}

	for _, k := range in.TagKeys {
		if n := utf8.RuneCountInString(k); n < 1 || n > 128 {
			return nil, awserror.Validation("Tag keys must contain 1 to 128 characters.")
		}
	}

	_, err := s.editTags(
		ctx,
		in.ResourceArn,
		func(tags map[string]string) (map[string]string, error) { return tagging.Remove(tags, in.TagKeys), nil },
	)
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{}, nil
}
