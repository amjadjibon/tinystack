package bedrockagentcorecontrol

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagentcorecontrol"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// tagged changes the tags of the runtime, endpoint or memory that arn names and returns the result.
func (s *Service) tagged(
	ctx context.Context,
	arn string,
	change func(map[string]string) (map[string]string, error),
) (map[string]string, error) {
	var tags map[string]string
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		// arn:aws:bedrock-agentcore:<region>:<account>:runtime/<id>[/runtime-endpoint/<name>] or memory/<id>
		missing := notFound("Resource %s does not exist", arn)
		rest, ok := strings.CutPrefix(arn, arnPrefix+scope.Region+":"+scope.AccountID+":")
		if !ok {
			return missing
		}

		parts := strings.Split(rest, "/")
		switch {
		case len(parts) == 2 && parts[0] == kindRuntime:
			r, e := readRuntime(tx, scope, parts[1])
			if e != nil {
				return missing
			}

			if r.Tags, e = change(r.Tags); e != nil {
				return e
			}

			tags = r.Tags

			return storage.Write(tx, runtimeKey(scope, r.ID), r)
		case len(parts) == 4 && parts[0] == kindRuntime && parts[2] == "runtime-endpoint":
			ep, e := readEndpoint(tx, scope, parts[1], parts[3])
			if e != nil {
				return missing
			}

			if ep.Tags, e = change(ep.Tags); e != nil {
				return e
			}

			tags = ep.Tags

			return storage.Write(tx, endpointKey(scope, parts[1], ep.Name), ep)
		case len(parts) == 2 && parts[0] == kindMemory:
			m, e := readMemory(tx, scope, parts[1])
			if e != nil {
				return missing
			}

			if m.Tags, e = change(m.Tags); e != nil {
				return e
			}

			tags = m.Tags

			return storage.Write(tx, memoryKey(scope, m.ID), m)
		}

		return missing
	})

	return tags, err
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	if err := tagging.CheckSet(in.Tags); err != nil {
		return nil, invalid("%s", err.Error())
	}

	_, err := s.tagged(ctx, common.Value(in.ResourceArn), func(current map[string]string) (map[string]string, error) {
		merged, err := tagging.Merge(current, in.Tags)
		if err != nil {
			return nil, invalid("%s", err.Error())
		}

		return merged, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	_, err := s.tagged(ctx, common.Value(in.ResourceArn), func(current map[string]string) (map[string]string, error) {
		return tagging.Remove(current, in.TagKeys), nil
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
	tags, err := s.tagged(
		ctx,
		common.Value(in.ResourceArn),
		func(current map[string]string) (map[string]string, error) {
			return current, nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{Tags: tags}, nil
}
