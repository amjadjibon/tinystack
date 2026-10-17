package lambda

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) AuthorizationResources(
	ctx context.Context,
	_ string,
	input any,
) ([]catalog.AuthorizationResource, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	ref := catalog.InputString(input, "FunctionName")
	if ref == "" {
		ref = catalog.InputString(input, "Resource")
	}

	if ref == "" {
		return nil, nil
	}

	name, qualifier, err := resolveName(&m, ref)
	if err != nil {
		return nil, err
	}

	param := catalog.InputString(input, "Qualifier")
	if param != "" {
		qualifier, err = withQualifier(qualifier, &param)
		if err != nil {
			return nil, err
		}
	}

	target := catalog.AuthorizationResource{ARN: qualifiedARN(m.Scope, name, qualifier), TagsKnown: true}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		fn, _, readErr := loadFunction(tx, m.Key("lambda", "function", name))
		if fn != nil {
			target.Tags = common.CloneTags(fn.Tags)
			target.OwnerAccount = m.AccountID
			data, _ := tx.Get(policyKey(m.Scope, name, qualifier))
			target.Policy = string(data)
		}

		return readErr
	})
	return []catalog.AuthorizationResource{target}, err
}
