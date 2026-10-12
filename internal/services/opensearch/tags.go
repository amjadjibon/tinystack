package opensearch

import (
	"context"
	"maps"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/opensearch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func parseDomainARN(arn string) (account, region, domainName string, err error) {
	parts := strings.Split(arn, ":")
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" || parts[2] != "es" {
		return "", "", "", invalid("Invalid ARN: %s", arn)
	}

	region = parts[3]
	account = parts[4]
	rest := parts[5]
	if !strings.HasPrefix(rest, "domain/") {
		return "", "", "", invalid("Invalid ARN: %s", arn)
	}

	domainName = strings.TrimPrefix(rest, "domain/")
	if domainName == "" {
		return "", "", "", invalid("Invalid ARN: %s", arn)
	}

	return account, region, domainName, nil
}

func (s *Service) AddTags(
	ctx context.Context,
	in *api.AddTagsRequest,
) (*api.AddTagsOutput, error) {
	if in == nil || in.ARN == nil || *in.ARN == "" {
		return nil, invalid("ARN is required")
	}

	account, region, name, err := parseDomainARN(*in.ARN)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.TagList)
	if err != nil {
		return nil, err
	}

	targetScope := request.Scope{AccountID: account, Region: region}
	key := targetScope.Key(serviceName, domainKind, name)

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, found, readErr := storage.Read[domainRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found || rec == nil {
			return notFound("Domain not found for ARN: %s", *in.ARN)
		}

		current := *rec
		merged := common.CloneTags(current.Tags)
		maps.Copy(merged, tags)
		if setErr := tagging.CheckSet(merged); setErr != nil {
			return checkTagLimit(setErr)
		}

		current.Tags = merged
		return storage.Write(tx, key, current)
	})
	if err != nil {
		return nil, err
	}

	return &api.AddTagsOutput{}, nil
}

func (s *Service) ListTags(
	ctx context.Context,
	in *api.ListTagsRequest,
) (*api.ListTagsResponse, error) {
	if in == nil || in.ARN == nil || *in.ARN == "" {
		return nil, invalid("ARN is required")
	}

	account, region, name, err := parseDomainARN(*in.ARN)
	if err != nil {
		return nil, err
	}

	targetScope := request.Scope{AccountID: account, Region: region}
	key := targetScope.Key(serviceName, domainKind, name)

	var rec *domainRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, readErr := storage.Read[domainRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found || r == nil {
			return notFound("Domain not found for ARN: %s", *in.ARN)
		}

		rec = r
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ListTagsResponse{TagList: tagList(rec.Tags)}, nil
}

func (s *Service) RemoveTags(
	ctx context.Context,
	in *api.RemoveTagsRequest,
) (*api.RemoveTagsOutput, error) {
	if in == nil || in.ARN == nil || *in.ARN == "" {
		return nil, invalid("ARN is required")
	}

	account, region, name, err := parseDomainARN(*in.ARN)
	if err != nil {
		return nil, err
	}

	targetScope := request.Scope{AccountID: account, Region: region}
	key := targetScope.Key(serviceName, domainKind, name)

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, found, readErr := storage.Read[domainRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found || rec == nil {
			return notFound("Domain not found for ARN: %s", *in.ARN)
		}

		current := *rec
		current.Tags = tagging.Remove(current.Tags, in.TagKeys)
		return storage.Write(tx, key, current)
	})
	if err != nil {
		return nil, err
	}

	return &api.RemoveTagsOutput{}, nil
}
