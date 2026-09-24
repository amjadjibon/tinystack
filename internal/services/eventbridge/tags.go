package eventbridge

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// tagMap converts EventBridge tags to a map, rejecting repeated keys.
func tagMap(tags []api.Tag) (map[string]string, error) {
	out := make(map[string]string, len(tags))
	for _, tag := range tags {
		if tag.Key == nil || tag.Value == nil {
			return nil, invalid("Tags must have a Key and a Value.")
		}

		if _, dup := out[*tag.Key]; dup {
			return nil, invalid("Duplicate tag key %s.", *tag.Key)
		}

		out[*tag.Key] = *tag.Value
	}

	return out, nil
}

// mergeTags adds tags to a rule's tag set within the tag limits.
func mergeTags(r *rule, add map[string]string) error {
	tags, err := tagging.Merge(r.Tags, add)
	if err != nil {
		return invalid("Invalid tags: %v.", err)
	}

	r.Tags = tags
	return nil
}

// resolveARN maps a rule ARN on the caller's default bus to its name and key.
func resolveARN(ctx context.Context, arn *string) (name, key string, err error) {
	m, err := identity(ctx)
	if err != nil {
		return "", "", err
	}

	name, ok := strings.CutPrefix(value(arn), ruleARN(m.Scope, ""))
	if !ok || !ruleName.MatchString(name) {
		return "", "", awserror.New("ResourceNotFoundException", "Resource "+value(arn)+" does not exist.")
	}

	return name, rulePrefix(m.Scope) + name, nil
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	name, key, err := resolveARN(ctx, in.ResourceARN)
	if err != nil {
		return nil, err
	}

	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.update(ctx, name, key, func(r *rule) error { return mergeTags(r, add) })
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	name, key, err := resolveARN(ctx, in.ResourceARN)
	if err != nil {
		return nil, err
	}

	err = s.update(ctx, name, key, func(r *rule) error {
		r.Tags = tagging.Remove(r.Tags, in.TagKeys)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	name, key, err := resolveARN(ctx, in.ResourceARN)
	if err != nil {
		return nil, err
	}

	var tags map[string]string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, ok, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		if !ok {
			return ruleNotFound(name)
		}

		tags = r.Tags
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := &api.ListTagsForResourceResponse{Tags: []api.Tag{}}
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		out.Tags = append(out.Tags, api.Tag{Key: new(k), Value: new(tags[k])})
	}

	return out, nil
}
