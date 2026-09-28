package ecs

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagMap(tags []api.Tag) map[string]string {
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		out[value(t.Key)] = value(t.Value)
	}

	return out
}

func tagList(tags map[string]string) []api.Tag {
	out := make([]api.Tag, 0, len(tags))
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		out = append(out, api.Tag{Key: new(k), Value: new(tags[k])})
	}

	return out
}

// checkTags applies the shared tag limits; keys starting with "aws:" are
// ECS's own.
func checkTags(tags []api.Tag) error {
	for _, t := range tags {
		if t.Key == nil || strings.HasPrefix(strings.ToLower(*t.Key), "aws:") {
			return invalidParameter("Tag keys are required and cannot start with aws:.")
		}
	}

	if err := tagging.Check(tagMap(tags)); err != nil {
		return invalidParameter("%s", err.Error())
	}

	return nil
}

// mergeTags returns base with add applied.
func mergeTags(base, add []api.Tag) []api.Tag {
	tags := tagMap(base)
	maps.Copy(tags, tagMap(add))
	return tagList(tags)
}

// tagged is a taggable record's tags and how to store the record.
type tagged struct {
	tags *[]api.Tag
	save func(storage.WriteTx) error
}

// taggable finds the resource an ARN names: a cluster, service, task
// definition, or task.
func taggable(tx storage.ReadTx, scope request.Scope, ref string) (*tagged, error) {
	notFound := awserror.New("ResourceNotFoundException", "The specified resource could not be found.")
	kind, rest, _ := strings.Cut(strings.TrimPrefix(ref, arn(scope, "")), "/")
	if !strings.HasPrefix(ref, arn(scope, "")) {
		return nil, invalidParameter("The ARN %s is not an ECS resource of this account and Region.", ref)
	}

	switch kind {
	case kindCluster:
		c, ok, err := read[cluster](tx, clusterKey(scope, rest))
		if err != nil || !ok || c.Status != active {
			return nil, errorsOr(err, clusterNotFound())
		}

		return &tagged{tags: &c.Tags, save: func(tx storage.WriteTx) error {
			return write(tx, clusterKey(scope, rest), c)
		}}, nil
	case kindService:
		clusterName, name, _ := strings.Cut(rest, "/")
		svc, ok, err := read[ecsService](tx, serviceKey(scope, clusterName, name))
		if err != nil || !ok || svc.Status == inactive {
			return nil, errorsOr(err, notFound)
		}

		return &tagged{tags: &svc.Tags, save: func(tx storage.WriteTx) error {
			return write(tx, serviceKey(scope, clusterName, name), svc)
		}}, nil
	case "task-definition":
		d, err := revisionRef(tx, scope, &ref)
		if err != nil {
			return nil, notFound
		}

		return &tagged{tags: &d.Tags, save: func(tx storage.WriteTx) error {
			return write(tx, taskDefKey(scope, *d.Def.Family, *d.Def.Revision), d)
		}}, nil
	case kindTask:
		clusterName, id, _ := strings.Cut(rest, "/")
		t, ok, err := read[task](tx, taskKey(scope, clusterName, id))
		if err != nil || !ok {
			return nil, errorsOr(err, notFound)
		}

		return &tagged{tags: &t.Tags, save: func(tx storage.WriteTx) error {
			return write(tx, taskKey(scope, clusterName, id), t)
		}}, nil
	}

	return nil, notFound
}

// retag runs fn on the tags of the resource arn names and stores it.
func (s *Service) retag(ctx context.Context, ref *string, fn func([]api.Tag) ([]api.Tag, error)) error {
	scope, err := identity(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, findErr := taggable(tx, scope, value(ref))
		if findErr != nil {
			return findErr
		}

		tags, fnErr := fn(*r.tags)
		if fnErr != nil {
			return fnErr
		}

		*r.tags = tags
		return r.save(tx)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags []api.Tag) ([]api.Tag, error) {
		merged := mergeTags(tags, in.Tags)
		if err := checkTags(merged); err != nil {
			return nil, err
		}

		return merged, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags []api.Tag) ([]api.Tag, error) {
		return tagList(tagging.Remove(tagMap(tags), in.TagKeys)), nil
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
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListTagsForResourceResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, findErr := taggable(tx, scope, value(in.ResourceArn))
		if findErr != nil {
			return findErr
		}

		out.Tags = append([]api.Tag{}, *r.tags...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
