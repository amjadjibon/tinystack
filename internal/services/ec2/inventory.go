package ec2

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// inventoried is the part of every taggable record the inventory reads.
type inventoried struct {
	Tags  map[string]string
	ID    string
	Name  string
	State string
	VPCID string
}

// Resources lists every taggable resource in every account and Region, for
// the resource browser and the Resource Groups Tagging API. A terminated
// instance and a deleted NAT gateway, which are only kept for visibility, are
// left out.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	kinds := make(map[string]taggableKind, len(taggable))
	for _, t := range taggable {
		kinds[t.kind] = t
	}

	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, "ec2")
			if !ok {
				continue
			}

			kind, _, _ := strings.Cut(rest, "/")
			t, ok := kinds[kind]
			if !ok {
				continue
			}

			r, _, err := storage.Read[inventoried](tx, k)
			if err != nil {
				return err
			}

			if (kind == kindInstance && r.State == stateTerminated) ||
				(kind == kindNATGateway && r.State == stateDeleted) ||
				(kind == kindTransitGateway && r.State == stateDeleted) ||
				(kind == kindTransitGatewayAttachment && r.State == stateDeleted) ||
				(kind == kindTransitGatewayRouteTable && r.State == stateDeleted) ||
				(kind == kindCustomerGateway && r.State == stateDeleted) ||
				(kind == kindVpnGateway && r.State == stateDeleted) ||
				(kind == kindVpnConnection && r.State == stateDeleted) ||
				(kind == kindPeering && (r.State == stateDeleted || r.State == "rejected" || r.State == "failed")) {
				continue
			}

			details := map[string]any{}
			if r.State != "" {
				details["State"] = r.State
			}

			if r.VPCID != "" {
				details["VPCID"] = r.VPCID
			}

			out = append(out, catalog.Resource{
				Type: t.resourceType, Account: account, Region: region, Name: cmp.Or(r.Name, r.ID),
				ARN:  fmt.Sprintf("arn:aws:ec2:%s:%s:%s/%s", region, account, t.resourceType, r.ID),
				Tags: r.Tags, Details: details,
			})
		}

		return nil
	})

	return out, err
}
