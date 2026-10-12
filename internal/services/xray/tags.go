package xray

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/xray"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) TagResource(
	ctx context.Context,
	in *api.TagResourceRequest,
) (*api.TagResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.ResourceARN == nil {
		return nil, tagError("ResourceARN is required")
	}

	tags, tagErr := tagMap(in.Tags)
	if tagErr != nil {
		return nil, tagErr
	}

	if checkErr := tagging.CheckSet(tags); checkErr != nil {
		return nil, checkTagLimit(checkErr)
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		currentTags, key, isGroup, findErr := findTaggedResource(tx, scope, in.ResourceARN)
		if findErr != nil {
			return findErr
		}

		merged, mergeErr := tagging.Merge(currentTags, tags)
		if mergeErr != nil {
			return checkTagLimit(mergeErr)
		}

		if checkErr := tagging.CheckSet(merged); checkErr != nil {
			return checkTagLimit(checkErr)
		}

		if isGroup {
			rec, _, readErr := storage.Read[groupRecord](tx, key)
			if readErr != nil {
				return readErr
			}

			rec.Tags = merged
			return storage.Write(tx, key, rec)
		}

		rec, _, readErr := storage.Read[samplingRuleRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		rec.Tags = merged
		return storage.Write(tx, key, rec)
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
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.ResourceARN == nil {
		return nil, tagError("ResourceARN is required")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		currentTags, key, isGroup, findErr := findTaggedResource(tx, scope, in.ResourceARN)
		if findErr != nil {
			return findErr
		}

		keys := append([]string(nil), in.TagKeys...)
		merged := tagging.Remove(currentTags, keys)

		if isGroup {
			rec, _, readErr := storage.Read[groupRecord](tx, key)
			if readErr != nil {
				return readErr
			}

			rec.Tags = merged
			return storage.Write(tx, key, rec)
		}

		rec, _, readErr := storage.Read[samplingRuleRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		rec.Tags = merged
		return storage.Write(tx, key, rec)
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
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.ResourceARN == nil {
		return nil, tagError("ResourceARN is required")
	}

	var out []api.Tag
	if viewErr := s.store.View(ctx, func(tx storage.ReadTx) error {
		currentTags, _, _, findErr := findTaggedResource(tx, scope, in.ResourceARN)
		if findErr != nil {
			return findErr
		}

		out = tagList(currentTags)
		return nil
	}); viewErr != nil {
		return nil, viewErr
	}

	return &api.ListTagsForResourceResponse{
		Tags: out,
	}, nil
}

func findTaggedResource(
	tx storage.ReadTx,
	scope request.Scope,
	arn *string,
) (tags map[string]string, key string, isGroup bool, err error) {
	if arn == nil || *arn == "" {
		return nil, "", false, tagError("ResourceARN is required")
	}

	if strings.Contains(*arn, ":group/") {
		prefix := scope.Prefix(serviceName, groupKind)
		for _, key := range tx.Keys(prefix) {
			rec, found, readErr := storage.Read[groupRecord](tx, key)
			if readErr != nil {
				return nil, "", false, readErr
			}

			if found && rec != nil && rec.Group != nil && rec.Group.GroupARN != nil && *rec.Group.GroupARN == *arn {
				return rec.Tags, key, true, nil
			}
		}

		return nil, "", false, notFound("Resource %s not found", *arn)
	}

	if strings.Contains(*arn, ":sampling-rule/") {
		prefix := scope.Prefix(serviceName, samplingRuleKind)
		for _, key := range tx.Keys(prefix) {
			rec, found, readErr := storage.Read[samplingRuleRecord](tx, key)
			if readErr != nil {
				return nil, "", false, readErr
			}

			if found && rec != nil && rec.Record != nil && rec.Record.SamplingRule != nil &&
				rec.Record.SamplingRule.RuleARN != nil && *rec.Record.SamplingRule.RuleARN == *arn {
				return rec.Tags, key, false, nil
			}
		}

		defaultArn := samplingRuleARN(scope.Region, scope.AccountID, defaultRuleName)
		if *arn == defaultArn {
			key := scope.Key(serviceName, samplingRuleKind, defaultRuleName)
			rec, found, readErr := storage.Read[samplingRuleRecord](tx, key)
			if readErr != nil {
				return nil, "", false, readErr
			}

			if !found || rec == nil {
				return map[string]string{}, key, false, nil
			}

			return rec.Tags, key, false, nil
		}

		return nil, "", false, notFound("Resource %s not found", *arn)
	}

	return nil, "", false, tagError("Unsupported resource ARN type")
}
