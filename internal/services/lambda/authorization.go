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

	target, err := s.functionResource(ctx, m.Scope, name, qualifier)
	return []catalog.AuthorizationResource{target}, err
}

// DeliveryResource describes the function another service invokes, with its resource policy. A function that does
// not exist has no owner, which leaves the delivery to fail on its own.
func (s *Service) DeliveryResource(ctx context.Context, functionARN string) (catalog.AuthorizationResource, error) {
	m, name, qualifier, err := functionOfARN(functionARN)
	if err != nil {
		return catalog.AuthorizationResource{}, err
	}

	return s.functionResource(ctx, m.Scope, name, qualifier)
}

func (s *Service) functionResource(
	ctx context.Context,
	scope request.Scope,
	name, qualifier string,
) (catalog.AuthorizationResource, error) {
	target := catalog.AuthorizationResource{ARN: qualifiedARN(scope, name, qualifier), TagsKnown: true}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		fn, _, readErr := loadFunction(tx, scope.Key("lambda", "function", name))
		if fn != nil {
			target.Tags = common.CloneTags(fn.Tags)
			target.OwnerAccount = scope.AccountID
			data, _ := tx.Get(policyKey(scope, name, qualifier))
			target.Policy = string(data)
		}

		return readErr
	})
	return target, err
}
