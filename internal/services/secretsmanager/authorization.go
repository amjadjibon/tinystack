package secretsmanager

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/secretsmanager"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) AuthorizationResources(
	ctx context.Context,
	operation string,
	input any,
) ([]catalog.AuthorizationResource, error) {
	if operation == "CreateSecret" {
		return nil, nil
	}

	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	ids := catalog.InputStrings(input, "SecretId")
	ids = append(ids, catalog.InputStrings(input, "SecretIdList")...)

	if in, ok := input.(*api.BatchGetSecretValueRequest); ok && len(in.Filters) > 0 {
		limit := int64(maxBatchSecrets)
		if in.MaxResults != nil {
			limit = *in.MaxResults
		}

		listed, listErr := s.ListSecrets(
			ctx,
			&api.ListSecretsRequest{Filters: in.Filters, MaxResults: &limit, NextToken: in.NextToken},
		)
		if listErr != nil {
			return nil, listErr
		}

		for i := range listed.SecretList {
			ids = append(ids, common.Value(listed.SecretList[i].ARN))
		}
	}

	var out []catalog.AuthorizationResource
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, id := range ids {
			if id == "" {
				continue
			}

			targetMetadata := m
			request.ReferenceScope(&targetMetadata, "secretsmanager", id)
			stored, readErr := find(tx, targetMetadata.Scope, &id, s.now())
			if readErr != nil {
				return readErr
			}

			action := ""
			if operation == "BatchGetSecretValue" {
				action = "secretsmanager:GetSecretValue"
			}

			out = append(out, catalog.AuthorizationResource{
				ARN: stored.arn(targetMetadata.Scope), Action: action,
				OwnerAccount: targetMetadata.AccountID, Policy: stored.Policy,
				Tags: common.CloneTags(stored.Tags), TagsKnown: true,
			})
		}

		return nil
	})
	if operation == "BatchGetSecretValue" {
		out = append(out, catalog.AuthorizationResource{ARN: "*"})
		if in, ok := input.(*api.BatchGetSecretValueRequest); ok && len(in.Filters) > 0 {
			out = append(out, catalog.AuthorizationResource{ARN: "*", Action: "secretsmanager:ListSecrets"})
		}
	}

	return out, err
}
