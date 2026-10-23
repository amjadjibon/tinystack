package amp

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/amp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type tagged interface {
	tagMap() map[string]string
	setTagMap(map[string]string)
}

func readTagged[T any, P interface {
	*T
	tagged
}](tx storage.ReadTx, k string) (tagged, bool, error) {
	r, found, err := storage.Read[T](tx, k)
	if err != nil || !found {
		return nil, found, err
	}

	return P(r), true, nil
}

func split(scope request.Scope, value *string) (kind, id string, err error) {
	invalid := awserror.Validation("%q is not a valid APS ARN.", common.Value(value))
	if value == nil {
		return "", "", invalid
	}

	parts := strings.SplitN(*value, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != arnNamespace {
		return "", "", invalid
	}

	if parts[3] != scope.Region || parts[4] != scope.AccountID {
		return "", "", notFound("Resource", *value)
	}

	res := parts[5]
	kind, id, ok := strings.Cut(res, "/")
	if !ok || id == "" || (kind != kindWorkspace && kind != kindRuleGroupsNamespace) {
		return "", "", invalid
	}

	return kind, id, nil
}

func (s *Service) editTags(
	ctx context.Context,
	value *string,
	change func(tags map[string]string) (map[string]string, error),
) (map[string]string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	kind, id, err := split(scope, value)
	if err != nil {
		return nil, err
	}

	var out map[string]string
	storageKey := key(scope, kind, id)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var r tagged
		var found bool
		var e error
		if kind == kindWorkspace {
			r, found, e = readTagged[workspaceRecord](tx, storageKey)
		} else {
			r, found, e = readTagged[ruleGroupsNamespaceRecord](tx, storageKey)
		}

		if e != nil {
			return e
		}

		if !found {
			return notFound(kind, *value)
		}

		if out, e = change(maps.Clone(r.tagMap())); e != nil {
			return e
		}

		r.setTagMap(out)
		return storage.Write(tx, storageKey, r)
	})

	return out, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	tags, err := s.editTags(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string, error) {
		return tags, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{Tags: tags}, nil
}

func (s *Service) TagResource(
	ctx context.Context,
	in *api.TagResourceRequest,
) (*api.TagResourceResponse, error) {
	if len(in.Tags) == 0 {
		return nil, awserror.Validation("Tags is required.")
	}

	if err := tagging.CheckSet(in.Tags); err != nil {
		return nil, awserror.Validation("%s", err.Error())
	}

	_, err := s.editTags(ctx, in.ResourceArn, func(current map[string]string) (map[string]string, error) {
		merged, e := tagging.Merge(current, in.Tags)
		if e != nil {
			return nil, awserror.Validation("%s", e.Error())
		}

		return merged, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(
	ctx context.Context,
	in *api.UntagResourceRequest,
) (*api.UntagResourceResponse, error) {
	if len(in.TagKeys) == 0 {
		return nil, awserror.Validation("tagKeys is required.")
	}

	_, err := s.editTags(ctx, in.ResourceArn, func(current map[string]string) (map[string]string, error) {
		return tagging.Remove(current, in.TagKeys), nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{}, nil
}
