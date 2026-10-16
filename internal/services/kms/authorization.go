package kms

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/kms"
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

	refs := []catalog.AuthorizationResource{}
	if in, ok := input.(*api.ReEncryptRequest); ok {
		source := common.Value(in.SourceKeyId)
		if source == "" {
			source, err = blobKeyID(in.CiphertextBlob)
			if err != nil {
				return nil, err
			}
		}

		// https://docs.aws.amazon.com/service-authorization/latest/reference/list_kms.html
		refs = append(refs, catalog.AuthorizationResource{ARN: source, Action: "kms:ReEncryptFrom"},
			catalog.AuthorizationResource{ARN: common.Value(in.DestinationKeyId), Action: "kms:ReEncryptTo"})
	} else {
		ref := catalog.InputString(input, "KeyId")
		if ref == "" {
			ref = catalog.InputString(input, "TargetKeyId")
		}

		if ref == "" {
			ref = catalog.InputString(input, "AliasName")
		}

		if in, ok := input.(*api.DecryptRequest); ok && ref == "" {
			ref, err = blobKeyID(in.CiphertextBlob)
			if err != nil {
				return nil, err
			}
		}

		if ref != "" {
			refs = append(refs, catalog.AuthorizationResource{ARN: ref})
		}
	}

	if len(refs) == 0 {
		return nil, nil
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for i := range refs {
			scope := referenceScope(ctx, m.Scope, refs[i].ARN)
			id, readErr := resolve(tx, scope, refs[i].ARN, true)
			if readErr != nil {
				return readErr
			}

			refs[i].ARN, refs[i].TagsKnown = keyARN(scope, id), true
			stored, _, readErr := storage.Read[key](tx, keyKey(scope, id))
			if readErr != nil {
				return readErr
			}

			if stored != nil {
				refs[i].Tags = common.CloneTags(stored.Tags)
				refs[i].OwnerAccount = scope.AccountID
				refs[i].Policy = stored.Policy
				refs[i].PolicyRequired = true
			}
		}

		return nil
	})
	return refs, err
}
