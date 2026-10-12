package verifiedpermissions

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/verifiedpermissions"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagError(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return awserror.New("TooManyTagsException", err.Error())
	}

	return awserror.Validation("%s", err.Error())
}

func (s *Service) TagResource(
	ctx context.Context,
	in *api.TagResourceInput,
) (*api.TagResourceOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.ResourceArn == nil || *in.ResourceArn == "" {
		return nil, awserror.Validation("resourceArn is required")
	}

	if len(in.Tags) == 0 {
		return nil, awserror.Validation("tags must not be empty")
	}

	if checkErr := tagging.CheckSet(in.Tags); checkErr != nil {
		return nil, tagError(checkErr)
	}

	id, err := parsePolicyStoreARN(scope, *in.ResourceArn)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, kindPolicyStore, id)
		rec, found, readErr := storage.Read[policyStoreRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found || rec == nil {
			return notFound(resourceTypePolicyStore, id, fmt.Sprintf("policy store %s not found", id))
		}

		merged := maps.Clone(rec.Tags)
		if merged == nil {
			merged = map[string]string{}
		}

		maps.Copy(merged, in.Tags)
		if chkErr := tagging.CheckSet(merged); chkErr != nil {
			return tagError(chkErr)
		}

		rec.Tags = merged
		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceOutput{}, nil
}

func (s *Service) UntagResource(
	ctx context.Context,
	in *api.UntagResourceInput,
) (*api.UntagResourceOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.ResourceArn == nil || *in.ResourceArn == "" {
		return nil, awserror.Validation("resourceArn is required")
	}

	if len(in.TagKeys) == 0 {
		return nil, awserror.Validation("tagKeys must not be empty")
	}

	id, err := parsePolicyStoreARN(scope, *in.ResourceArn)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, kindPolicyStore, id)
		rec, found, readErr := storage.Read[policyStoreRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found || rec == nil {
			return notFound(resourceTypePolicyStore, id, fmt.Sprintf("policy store %s not found", id))
		}

		rec.Tags = tagging.Remove(rec.Tags, in.TagKeys)
		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceOutput{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceInput,
) (*api.ListTagsForResourceOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.ResourceArn == nil || *in.ResourceArn == "" {
		return nil, awserror.Validation("resourceArn is required")
	}

	id, err := parsePolicyStoreARN(scope, *in.ResourceArn)
	if err != nil {
		return nil, err
	}

	var tags map[string]string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		key := scope.Key(serviceName, kindPolicyStore, id)
		rec, found, readErr := storage.Read[policyStoreRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found || rec == nil {
			return notFound(resourceTypePolicyStore, id, fmt.Sprintf("policy store %s not found", id))
		}

		tags = common.CloneTags(rec.Tags)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceOutput{
		Tags: tags,
	}, nil
}
