package route53resolver

import (
	"context"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/route53resolver"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateResolverRule(
	ctx context.Context,
	in *api.CreateResolverRuleRequest,
) (*api.CreateResolverRuleResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalid("Input is required")
	}

	if in.TargetIps != nil && len(in.TargetIps) == 0 {
		return nil, invalid("TargetIps must contain at least one target")
	}

	in = common.Clone(in)
	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if validationErr := checkString(in.DomainName, 1, 256, "DomainName"); validationErr != nil {
		return nil, validationErr
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		existing, original, e := tokenRecord(tx, scope, ruleKind, in.CreatorRequestId, in)
		if e != nil {
			return e
		}

		if existing != nil {
			r = existing
			return nil
		}

		id := "rslvr-rr-" + common.RandomID(17, "0123456789abcdef")
		now := time.Now().UTC().Format(time.RFC3339Nano)
		domain := *in.DomainName
		if !strings.HasSuffix(domain, ".") {
			domain += "."
		}

		r = &record{
			ID: id, Kind: ruleKind, CreateRequest: original, Tags: tags,
			Rule: &api.ResolverRule{
				Id:                 new(id),
				Arn:                new(arn(scope, ruleKind, id)),
				CreatorRequestId:   in.CreatorRequestId,
				Name:               in.Name,
				RuleType:           in.RuleType,
				DomainName:         new(domain),
				ResolverEndpointId: in.ResolverEndpointId,
				TargetIps:          in.TargetIps,
				Status:             new("COMPLETE"),
				OwnerId:            new(scope.AccountID),
				ShareStatus:        new("NOT_SHARED"),
				CreationTime:       new(now),
				ModificationTime:   new(now),
			},
		}

		if e := checkRule(tx, scope, r.Rule); e != nil {
			return e
		}

		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateResolverRuleResponse{ResolverRule: r.Rule}, nil
}

func (s *Service) UpdateResolverRule(
	ctx context.Context,
	in *api.UpdateResolverRuleRequest,
) (*api.UpdateResolverRuleResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.Config == nil {
		return nil, invalid("Config is required")
	}

	if in.Config.TargetIps != nil && len(in.Config.TargetIps) == 0 {
		return nil, invalid("TargetIps must contain at least one target")
	}

	in = common.Clone(in)
	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = read(tx, scope, ruleKind, in.ResolverRuleId)
		if e != nil {
			return e
		}

		if in.Config.Name != nil {
			r.Rule.Name = in.Config.Name
		}

		if in.Config.TargetIps != nil {
			r.Rule.TargetIps = in.Config.TargetIps
		}

		if in.Config.ResolverEndpointId != nil {
			r.Rule.ResolverEndpointId = in.Config.ResolverEndpointId
		}

		if e := checkRule(tx, scope, r.Rule); e != nil {
			return e
		}

		r.Rule.ModificationTime = new(time.Now().UTC().Format(time.RFC3339Nano))
		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateResolverRuleResponse{ResolverRule: r.Rule}, nil
}

func (s *Service) DeleteResolverRule(
	ctx context.Context,
	in *api.DeleteResolverRuleRequest,
) (*api.DeleteResolverRuleResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalid("Input is required")
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = read(tx, scope, ruleKind, in.ResolverRuleId)
		if e != nil {
			return e
		}

		associations, e := storage.Records[record](tx, scope.Prefix(serviceName, associationKind))
		if e != nil {
			return e
		}

		for _, association := range associations {
			if common.Value(association.Association.ResolverRuleId) == r.ID {
				return awserror.New("ResourceInUseException", "Rule has VPC associations")
			}
		}

		r.Rule.Status = new("DELETING")
		tx.Delete(scope.Key(serviceName, ruleKind, r.ID))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteResolverRuleResponse{ResolverRule: r.Rule}, nil
}
