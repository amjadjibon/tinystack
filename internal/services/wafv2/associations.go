package wafv2

import (
	"context"
	"encoding/base64"
	"regexp"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/wafv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type association struct{ Resource, WebACL, Type string }

var (
	albARN = regexp.MustCompile(
		`^arn:aws:elasticloadbalancing:([a-z0-9-]+):([0-9]{12}):loadbalancer/app/([A-Za-z0-9-]{1,32})/([0-9a-f]{16})$`,
	)
	stageARN = regexp.MustCompile(
		`^arn:aws:apigateway:([a-z0-9-]+)::/restapis/([a-z0-9]{10})/stages/([A-Za-z0-9_-]{1,128})$`,
	)
)

// These targets are format-validated metadata. Their owning services are not
// queried, including for accountless API Gateway stage ARNs; storage is caller-scoped.
func target(scope request.Scope, value string) (string, error) {
	if m := albARN.FindStringSubmatch(value); m != nil {
		if m[1] != scope.Region || m[2] != scope.AccountID {
			return "", missing()
		}
		return "APPLICATION_LOAD_BALANCER", nil
	}
	if m := stageARN.FindStringSubmatch(value); m != nil {
		if m[1] != scope.Region {
			return "", missing()
		}
		return "API_GATEWAY", nil
	}
	return "", invalid("ResourceArn supports only regional ALB or API Gateway REST stage ARN formats")
}

func associationKey(scope request.Scope, value string) string {
	return scope.Key(serviceName, "association", base64.RawURLEncoding.EncodeToString([]byte(value)))
}

func (s *Service) AssociateWebACL(
	ctx context.Context,
	in *api.AssociateWebACLRequest,
) (*api.AssociateWebACLResponse, error) {
	scope, err := scopeFor(ctx, new(regional))
	if err != nil {
		return nil, err
	}
	value := common.Value(in.ResourceArn)
	kind, err := target(scope, value)
	if err != nil {
		return nil, err
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := byARN(tx, scope, common.Value(in.WebACLArn), kindACL, regional)
		if e != nil {
			return e
		}
		return storage.Write(tx, associationKey(scope, value), &association{Resource: value, WebACL: r.ARN, Type: kind})
	})
	return &api.AssociateWebACLResponse{}, err
}

func (s *Service) DisassociateWebACL(
	ctx context.Context,
	in *api.DisassociateWebACLRequest,
) (*api.DisassociateWebACLResponse, error) {
	scope, err := scopeFor(ctx, new(regional))
	if err != nil {
		return nil, err
	}
	value := common.Value(in.ResourceArn)
	if _, err = target(scope, value); err != nil {
		return nil, err
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error { tx.Delete(associationKey(scope, value)); return nil })
	return &api.DisassociateWebACLResponse{}, err
}

func (s *Service) GetWebACLForResource(
	ctx context.Context,
	in *api.GetWebACLForResourceRequest,
) (*api.GetWebACLForResourceResponse, error) {
	scope, err := scopeFor(ctx, new(regional))
	if err != nil {
		return nil, err
	}
	value := common.Value(in.ResourceArn)
	if _, err = target(scope, value); err != nil {
		return nil, err
	}
	out := &api.GetWebACLForResourceResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		a, ok, e := storage.Read[association](tx, associationKey(scope, value))
		if e != nil {
			return e
		}
		if !ok {
			return nil
		}
		r, e := byARN(tx, scope, a.WebACL, kindACL, regional)
		if e != nil {
			return e
		}
		out.WebACL = valueWebACL(r)
		return nil
	})
	return out, err
}

func (s *Service) ListResourcesForWebACL(
	ctx context.Context,
	in *api.ListResourcesForWebACLRequest,
) (*api.ListResourcesForWebACLResponse, error) {
	scope, err := scopeFor(ctx, new(regional))
	if err != nil {
		return nil, err
	}
	kind := common.Value(common.OrElse(in.ResourceType, new("APPLICATION_LOAD_BALANCER")))
	if kind != "APPLICATION_LOAD_BALANCER" && kind != "API_GATEWAY" {
		return nil, invalid("ResourceType supports APPLICATION_LOAD_BALANCER and API_GATEWAY")
	}
	values := []string{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := byARN(tx, scope, common.Value(in.WebACLArn), kindACL, regional)
		if e != nil {
			return e
		}
		rows, e := storage.Records[association](tx, scope.Prefix(serviceName, "association"))
		if e != nil {
			return e
		}
		for _, a := range rows {
			if a.WebACL == r.ARN && a.Type == kind {
				values = append(values, a.Resource)
			}
		}
		return nil
	})
	slices.Sort(values)
	return &api.ListResourcesForWebACLResponse{ResourceArns: values}, err
}
