package eventbridge

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// tagMap converts EventBridge tags to a map, rejecting repeated keys.
func tagMap(tags []api.Tag) (map[string]string, error) {
	return tagging.Map(
		tags,
		func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		invalid("Tags must have a Key and a Value."),
		func(k string) error { return invalid("Duplicate tag key %s.", k) },
	)
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

// tagged is the tag set of a rule or custom bus, and how to save it.
type tagged struct {
	tags *map[string]string
	save func(storage.WriteTx) error
}

// resolveTagged loads the rule or custom bus a tagging ARN names.
func resolveTagged(ctx context.Context, tx storage.ReadTx, arn *string) (*tagged, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	missing := awserror.New("ResourceNotFoundException", "Resource "+common.Value(arn)+" does not exist.")
	if name, ok := strings.CutPrefix(common.Value(arn), busARN(m.Scope, "")); ok {
		if !busPattern.MatchString(name) || name == defaultBus {
			return nil, missing
		}

		b, found, loadErr := loadBus(tx, m.Scope, name)
		if loadErr != nil || !found {
			if loadErr != nil {
				return nil, loadErr
			}

			return nil, missing
		}

		return &tagged{tags: &b.Tags, save: func(w storage.WriteTx) error { return saveBus(w, m.Scope, b) }}, nil
	}

	rest, ok := strings.CutPrefix(common.Value(arn), ruleARN(m.Scope, defaultBus, ""))
	if !ok {
		return nil, missing
	}

	busRef, name, onBus := strings.Cut(rest, "/")
	if !onBus {
		busRef, name = "", rest
	}

	ref, err := resolveRule(ctx, &name, &busRef)
	if err != nil {
		return nil, missing
	}

	r, err := loadRule(tx, &ref)
	if err != nil {
		return nil, err
	}

	return &tagged{tags: &r.Tags, save: func(w storage.WriteTx) error { return storage.Write(w, ref.key, r) }}, nil
}

// retag applies fn to the tags of the rule or custom bus arn names.
func (s *Service) retag(ctx context.Context, arn *string, fn func(map[string]string) (map[string]string, error)) error {
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, err := resolveTagged(ctx, tx, arn)
		if err != nil {
			return err
		}

		tags, err := fn(*t.tags)
		if err != nil {
			return err
		}

		*t.tags = tags
		return t.save(tx)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.retag(ctx, in.ResourceARN, func(tags map[string]string) (map[string]string, error) {
		merged, mergeErr := tagging.Merge(tags, add)
		if mergeErr != nil {
			return nil, invalid("Invalid tags: %v.", mergeErr)
		}

		return merged, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	err := s.retag(ctx, in.ResourceARN, func(tags map[string]string) (map[string]string, error) {
		return tagging.Remove(tags, in.TagKeys), nil
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
	var tags map[string]string
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		t, err := resolveTagged(ctx, tx, in.ResourceARN)
		if err == nil {
			tags = *t.tags
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{
		Tags: tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: &k, Value: &v} }),
	}, nil
}
