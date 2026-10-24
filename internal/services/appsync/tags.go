package appsync

import (
	"context"
	"maps"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/appsync"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func checkTags(tags map[string]string) error {
	if err := tagging.CheckSet(tags); err != nil {
		return badRequest("%s", err.Error())
	}

	return nil
}

// tagged reads the API an ARN (arn:aws:appsync:<region>:<account>:apis/<ID>)
// names, the only resource AppSync tags.
func tagged(tx storage.ReadTx, scope request.Scope, arn *string) (*api.GraphqlApi, error) {
	id, ok := strings.CutPrefix(common.Value(arn), apiARN(scope, ""))
	if !ok || strings.Contains(id, "/") {
		return nil, badRequest("Invalid resource ARN %s.", common.Value(arn))
	}

	return loadAPI(tx, scope, &id)
}

// retag replaces the tags of the API arn names with fn's result.
func (s *Service) retag(ctx context.Context, arn *string, fn func(map[string]string) map[string]string) error {
	return s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		id, isAPI := strings.CutPrefix(common.Value(arn), apiARN(scope, ""))
		if isAPI && strings.Contains(id, "/channelNamespace/") {
			apiID, name, _ := strings.Cut(id, "/channelNamespace/")
			n, err := loadNamespace(tx, scope, new(apiID), new(name))
			if err != nil {
				return err
			}

			n.Tags = fn(common.CloneTags(n.Tags))
			if err := checkTags(n.Tags); err != nil {
				return err
			}

			return storage.Write(tx, owned(scope, kindNamespace, apiID, name), n)
		}

		if isAPI && !strings.Contains(id, "/") {
			a, ok, err := storage.Read[api.Api](tx, scope.Key(service, kindEventAPI, id))
			if err != nil {
				return err
			}

			if ok {
				a.Tags = fn(common.CloneTags(a.Tags))
				if err := checkTags(a.Tags); err != nil {
					return err
				}

				return storage.Write(tx, scope.Key(service, kindEventAPI, id), a)
			}
		}

		a, err := tagged(tx, scope, arn)
		if err != nil {
			return err
		}

		a.Tags = fn(common.CloneTags(a.Tags))
		if checkErr := checkTags(a.Tags); checkErr != nil {
			return checkErr
		}

		return storage.Write(tx, scope.Key(service, kindAPI, *a.ApiId), a)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags map[string]string) map[string]string {
		maps.Copy(tags, in.Tags)
		return tags
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags map[string]string) map[string]string {
		return tagging.Remove(tags, in.TagKeys)
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
	out := &api.ListTagsForResourceResponse{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		id, isAPI := strings.CutPrefix(common.Value(in.ResourceArn), apiARN(scope, ""))
		if isAPI && strings.Contains(id, "/channelNamespace/") {
			apiID, name, _ := strings.Cut(id, "/channelNamespace/")
			n, err := loadNamespace(tx, scope, new(apiID), new(name))
			if err != nil {
				return err
			}

			out.Tags = common.CloneTags(n.Tags)
			return nil
		}

		if isAPI && !strings.Contains(id, "/") {
			a, ok, err := storage.Read[api.Api](tx, scope.Key(service, kindEventAPI, id))
			if err != nil {
				return err
			}

			if ok {
				out.Tags = common.CloneTags(a.Tags)
				return nil
			}
		}

		a, err := tagged(tx, scope, in.ResourceArn)
		if err != nil {
			return err
		}

		out.Tags = common.CloneTags(a.Tags)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
