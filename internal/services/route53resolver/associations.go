package route53resolver

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/route53resolver"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) AssociateResolverRule(
	ctx context.Context,
	in *api.AssociateResolverRuleRequest,
) (*api.AssociateResolverRuleResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalid("Input is required")
	}

	in = common.Clone(in)
	if validationErr := checkName(in.Name); validationErr != nil {
		return nil, validationErr
	}

	if validationErr := checkString(in.VPCId, 1, 64, "VPCId"); validationErr != nil {
		return nil, validationErr
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rule, e := read(tx, scope, ruleKind, in.ResolverRuleId)
		if e != nil {
			return e
		}

		if _, ok := tx.Get(scope.Key("ec2", "vpc", *in.VPCId)); !ok {
			return missing()
		}

		associations, e := storage.Records[record](tx, scope.Prefix(serviceName, associationKind))
		if e != nil {
			return e
		}

		for _, a := range associations {
			if common.Value(a.Association.VPCId) != *in.VPCId {
				continue
			}

			if common.Value(a.Association.ResolverRuleId) == rule.ID {
				return exists()
			}

			other, e := read(tx, scope, ruleKind, a.Association.ResolverRuleId)
			if e != nil {
				return e
			}

			if strings.EqualFold(common.Value(other.Rule.DomainName), common.Value(rule.Rule.DomainName)) {
				return exists()
			}
		}

		id := "rslvr-rrassoc-" + common.RandomID(17, "0123456789abcdef")
		r = &record{ID: id, Kind: associationKind, Association: &api.ResolverRuleAssociation{
			Id: new(id), Name: in.Name, ResolverRuleId: in.ResolverRuleId, VPCId: in.VPCId, Status: new("COMPLETE"),
		}}
		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.AssociateResolverRuleResponse{ResolverRuleAssociation: r.Association}, nil
}

func (s *Service) DisassociateResolverRule(
	ctx context.Context,
	in *api.DisassociateResolverRuleRequest,
) (*api.DisassociateResolverRuleResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalid("Input is required")
	}

	if validationErr := checkString(in.ResolverRuleId, 1, 64, "ResolverRuleId"); validationErr != nil {
		return nil, validationErr
	}

	if validationErr := checkString(in.VPCId, 1, 64, "VPCId"); validationErr != nil {
		return nil, validationErr
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		associations, e := storage.Records[record](tx, scope.Prefix(serviceName, associationKind))
		if e != nil {
			return e
		}

		for _, a := range associations {
			if common.Value(a.Association.VPCId) == *in.VPCId &&
				common.Value(a.Association.ResolverRuleId) == *in.ResolverRuleId {
				r = a
				r.Association.Status = new("DELETING")
				tx.Delete(scope.Key(serviceName, associationKind, r.ID))
				return nil
			}
		}

		return missing()
	})
	if err != nil {
		return nil, err
	}

	return &api.DisassociateResolverRuleResponse{ResolverRuleAssociation: r.Association}, nil
}
