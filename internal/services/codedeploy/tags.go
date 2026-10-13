package codedeploy

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codedeploy"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagMap(tags []api.Tag) (map[string]string, error) {
	for _, t := range tags {
		if t.Key == nil {
			return nil, awserror.New("InvalidTagsToAddException", "Tag key is required")
		}
	}

	out, err := tagging.Map(tags, func(t api.Tag) (*string, *string) { return t.Key, t.Value }, nil,
		func(string) error { return awserror.New("InvalidTagsToAddException", "Duplicate tag key") })
	if err != nil {
		return nil, err
	}

	if validationErr := tagging.CheckSet(out); validationErr != nil {
		return nil, awserror.New("InvalidTagsToAddException", validationErr.Error())
	}

	return out, nil
}

// The CodeDeploy quotas table specifies 10 tags per deployment group.
// Application tags use the shared tagging limit.
func checkGroupTags(tags map[string]string) error {
	if len(tags) > 10 {
		return awserror.New("InvalidTagsToAddException", "Deployment groups allow at most 10 tags")
	}

	return nil
}

func taggedRecord(tx storage.ReadTx, scope request.Scope, arn *string) (*record, string, error) {
	if arn == nil || *arn == "" {
		return nil, "", awserror.New("ResourceArnRequiredException", "ResourceArn is required")
	}

	if utf8.RuneCountInString(*arn) > 1011 {
		return nil, "", awserror.New("InvalidArnException", "ARN exceeds 1011 characters")
	}

	prefix := resourceARN(scope, "", "")
	prefix = strings.TrimSuffix(prefix, ":")
	rest, ok := strings.CutPrefix(*arn, prefix)
	if !ok {
		return nil, "", awserror.New("InvalidArnException", "ARN must belong to this account and Region")
	}

	kind, name, ok := strings.Cut(rest, ":")
	if !ok {
		return nil, "", awserror.New("InvalidArnException", "Invalid resource ARN")
	}

	switch kind {
	case "application":
		r, e := readApplication(tx, scope, new(name))
		return r, scope.Key(serviceName, applicationKind, name), e
	case "deploymentgroup":
		app, group, ok := strings.Cut(name, "/")
		if !ok {
			return nil, "", awserror.New("InvalidArnException", "Deployment group ARN requires app/group")
		}

		r, e := readGroup(tx, scope, new(app), new(group))
		return r, scope.Key(serviceName, groupKind, name), e
	default:
		return nil, "", awserror.New(
			"ArnNotSupportedException",
			"Only application and deployment group ARNs are supported",
		)
	}
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		in = &api.TagResourceInput{}
	}

	if len(in.Tags) == 0 {
		return nil, awserror.New("TagRequiredException", "Tags are required")
	}

	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, key, e := taggedRecord(tx, scope, in.ResourceArn)
		if e != nil {
			return e
		}

		merged, e := tagging.Merge(r.Tags, add)
		if e != nil {
			return awserror.New("InvalidTagsToAddException", e.Error())
		}

		if r.Group != nil {
			if validationErr := checkGroupTags(merged); validationErr != nil {
				return validationErr
			}
		}

		r.Tags = merged
		return storage.Write(tx, key, r)
	})
	return &api.TagResourceOutput{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		in = &api.UntagResourceInput{}
	}

	if len(in.TagKeys) == 0 {
		return nil, awserror.New("TagRequiredException", "TagKeys are required")
	}

	for _, key := range in.TagKeys {
		if validationErr := tagging.CheckSet(map[string]string{key: ""}); validationErr != nil {
			return nil, awserror.New("InvalidTagsToAddException", validationErr.Error())
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, key, e := taggedRecord(tx, scope, in.ResourceArn)
		if e != nil {
			return e
		}

		r.Tags = tagging.Remove(r.Tags, in.TagKeys)
		return storage.Write(tx, key, r)
	})
	return &api.UntagResourceOutput{}, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceInput,
) (*api.ListTagsForResourceOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		in = &api.ListTagsForResourceInput{}
	}

	var out []api.Tag
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, _, e := taggedRecord(tx, scope, in.ResourceArn)
		if e != nil {
			return e
		}

		out = tagging.List(r.Tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
		return nil
	})
	return &api.ListTagsForResourceOutput{Tags: out}, err
}
