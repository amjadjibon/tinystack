package ssm

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) AuthorizationResources(
	ctx context.Context,
	operation string,
	input any,
) ([]catalog.AuthorizationResource, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	names := catalog.InputStrings(input, "Name")
	names = append(names, catalog.InputStrings(input, "Names")...)
	if operation == "GetParametersByPath" {
		names = []string{catalog.InputString(input, "Path")}
	}

	if operation == "AddTagsToResource" || operation == "RemoveTagsFromResource" || operation == "ListTagsForResource" {
		if catalog.InputString(input, "ResourceType") == "Parameter" {
			names = []string{catalog.InputString(input, "ResourceId")}
		}
	}

	var out []catalog.AuthorizationResource
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, name := range names {
			if name == "" {
				continue
			}

			if !strings.HasPrefix(name, "arn:") {
				name, _, _ = strings.Cut(name, ":")
			}

			arn := name
			if !strings.HasPrefix(arn, "arn:") {
				arn = parameterARN(m.Scope, name)
			} else {
				_, name, _ = strings.Cut(arn, ":parameter/")
				name = "/" + name
			}

			target := catalog.AuthorizationResource{ARN: arn, TagsKnown: true}
			stored, _, readErr := storage.Read[parameter](tx, parameterKey(m.Scope, name))
			if readErr != nil {
				return readErr
			}

			if stored != nil {
				target.Tags = common.CloneTags(stored.Tags)
			}

			out = append(out, target)
		}

		return nil
	})
	return out, err
}
