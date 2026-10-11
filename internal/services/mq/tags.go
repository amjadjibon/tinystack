package mq

import (
	"context"
	"maps"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mq"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func arnID(scope request.Scope, arn string) (string, error) {
	parts := strings.Split(arn, ":")
	if len(parts) != 8 || parts[0] != "arn" || parts[1] != "aws" || parts[2] != serviceName ||
		parts[3] == "" || len(parts[4]) != 12 || parts[5] != brokerKind ||
		!namePattern.MatchString(parts[6]) || !idPattern.MatchString(parts[7]) {
		return "", invalid("ResourceArn must be a commercial Amazon MQ broker ARN")
	}
	for _, c := range parts[4] {
		if c < '0' || c > '9' {
			return "", invalid("Invalid ResourceArn account")
		}
	}
	if parts[3] != scope.Region || parts[4] != scope.AccountID {
		return "", missing()
	}
	return parts[7], nil
}

func tagRead(tx storage.ReadTx, scope request.Scope, arn string) (*record, error) {
	id, e := arnID(scope, arn)
	if e != nil {
		return nil, e
	}
	r, e := read(tx, scope, id)
	if e != nil {
		return nil, e
	}
	if *r.Broker.BrokerArn != arn {
		return nil, missing()
	}
	return r, nil
}

func (s *Service) CreateTags(ctx context.Context, in *api.CreateTagsRequest) (*api.CreateTagsOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	tags := maps.Clone(in.Tags)
	if e = tagging.CheckSet(tags); e != nil {
		return nil, invalid(e.Error())
	}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := tagRead(tx, scope, common.Value(in.ResourceArn))
		if err != nil {
			return err
		}
		merged, err := tagging.Merge(r.Broker.Tags, tags)
		if err != nil {
			return invalid(err.Error())
		}
		r.Broker.Tags = merged
		return save(tx, scope, r)
	})
	if e != nil {
		return nil, e
	}
	return &api.CreateTagsOutput{}, nil
}

func (s *Service) ListTags(ctx context.Context, in *api.ListTagsRequest) (*api.ListTagsResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	var tags map[string]string
	e = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, err := tagRead(tx, scope, common.Value(in.ResourceArn))
		if err == nil {
			tags = maps.Clone(r.Broker.Tags)
		}
		return err
	})
	if e != nil {
		return nil, e
	}
	return &api.ListTagsResponse{Tags: tags}, nil
}

func (s *Service) DeleteTags(ctx context.Context, in *api.DeleteTagsRequest) (*api.DeleteTagsOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in.TagKeys == nil || len(in.TagKeys) > 50 {
		return nil, invalid("TagKeys requires a list of at most 50 keys")
	}
	seen := map[string]string{}
	for _, k := range in.TagKeys {
		if _, ok := seen[k]; ok {
			return nil, invalid("Duplicate TagKeys")
		}
		seen[k] = ""
	}
	if e = tagging.CheckSet(seen); e != nil {
		return nil, invalid(e.Error())
	}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := tagRead(tx, scope, common.Value(in.ResourceArn))
		if err != nil {
			return err
		}
		r.Broker.Tags = tagging.Remove(r.Broker.Tags, in.TagKeys)
		return save(tx, scope, r)
	})
	if e != nil {
		return nil, e
	}
	return &api.DeleteTagsOutput{}, nil
}
