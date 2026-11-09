package lambda

import (
	"context"
	"encoding/json"

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

	if ref := catalog.InputString(input, "LayerName"); ref != "" {
		scope, name, resolveErr := layerTarget(m, ref)
		if resolveErr != nil {
			return nil, resolveErr
		}

		var version int64
		// Numeric input members are read through JSON for the authorization target.
		data, marshalErr := json.Marshal(input)
		if marshalErr != nil {
			return nil, marshalErr
		}

		var fields struct{ VersionNumber int64 }
		if decodeErr := json.Unmarshal(data, &fields); decodeErr != nil {
			return nil, decodeErr
		}

		version = fields.VersionNumber
		return s.layerAuthorization(ctx, scope, name, version, operation)
	}

	if operation == "GetLayerVersionByArn" {
		scope, name, version, parseErr := parseLayerVersionARN(m, catalog.InputString(input, "Arn"))
		if parseErr != nil {
			return nil, parseErr
		}

		return s.layerAuthorization(ctx, scope, name, version, operation)
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

func (s *Service) layerAuthorization(
	ctx context.Context,
	scope request.Scope,
	name string,
	version int64,
	operation string,
) ([]catalog.AuthorizationResource, error) {
	if version == 0 {
		return nil, nil
	}

	target := catalog.AuthorizationResource{OwnerAccount: scope.AccountID, TagsKnown: true}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		l, loadErr := loadLayer(tx, scope, name, version)
		if loadErr != nil {
			return loadErr
		}

		data, marshalErr := json.Marshal(l.Policy)
		target.ARN, target.Policy = l.versionARN(scope), string(data)
		if operation == "GetLayerVersionByArn" {
			target.Action = layerReadAction
		}

		return marshalErr
	})
	return []catalog.AuthorizationResource{target}, err
}
