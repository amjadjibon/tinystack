package opensearchserverless

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/opensearchserverless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func readSettings(tx storage.ReadTx, scope request.Scope) (*api.AccountSettingsDetail, error) {
	r, found, err := storage.Read[api.AccountSettingsDetail](tx, scope.Key(serviceName, "settings", "account"))
	if err != nil {
		return nil, err
	}

	if !found {
		r = &api.AccountSettingsDetail{CapacityLimits: &api.CapacityLimits{
			MaxIndexingCapacityInOCU: new(int64(10)), MaxSearchCapacityInOCU: new(int64(10)),
		}}
	}

	return r, nil
}

func (s *Service) GetAccountSettings(
	ctx context.Context,
	_ *api.GetAccountSettingsRequest,
) (*api.GetAccountSettingsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *api.AccountSettingsDetail
	err = s.store.View(ctx, func(tx storage.ReadTx) error { var e error; r, e = readSettings(tx, scope); return e })
	return &api.GetAccountSettingsResponse{AccountSettingsDetail: r}, err
}

func (s *Service) UpdateAccountSettings(
	ctx context.Context,
	in *api.UpdateAccountSettingsRequest,
) (*api.UpdateAccountSettingsResponse, error) {
	if in.CapacityLimits == nil {
		return nil, awserror.Validation("capacityLimits is required.")
	}

	for _, v := range []*int64{in.CapacityLimits.MaxIndexingCapacityInOCU, in.CapacityLimits.MaxSearchCapacityInOCU} {
		if v != nil && (*v < 2 || *v > 1700) {
			return nil, awserror.Validation("Capacity must be between 2 and 1700.")
		}
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *api.AccountSettingsDetail
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = readSettings(tx, scope)
		if e != nil {
			return e
		}

		if v := in.CapacityLimits.MaxIndexingCapacityInOCU; v != nil {
			r.CapacityLimits.MaxIndexingCapacityInOCU = v
		}

		if v := in.CapacityLimits.MaxSearchCapacityInOCU; v != nil {
			r.CapacityLimits.MaxSearchCapacityInOCU = v
		}

		return storage.Write(tx, scope.Key(serviceName, "settings", "account"), r)
	})
	return &api.UpdateAccountSettingsResponse{AccountSettingsDetail: r}, err
}

func (s *Service) GetPoliciesStats(
	ctx context.Context,
	_ *api.GetPoliciesStatsRequest,
) (*api.GetPoliciesStatsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var encryption, network, data int64
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, family := range []string{"security", "access"} {
			rows, e := storage.Records[policy](tx, scope.Prefix(serviceName, family))
			if e != nil {
				return e
			}

			for _, p := range rows {
				switch common.Value(p.Type) {
				case encryptionType:
					encryption++
				case networkType:
					network++
				case dataType:
					data++
				}
			}
		}

		return nil
	})
	return &api.GetPoliciesStatsResponse{
		AccessPolicyStats: &api.AccessPolicyStats{DataPolicyCount: new(data)},
		SecurityPolicyStats: &api.SecurityPolicyStats{
			EncryptionPolicyCount: new(encryption),
			NetworkPolicyCount:    new(network),
		},
		SecurityConfigStats: &api.SecurityConfigStats{SamlConfigCount: new(int64(0))},
		LifecyclePolicyStats: &api.LifecyclePolicyStats{
			RetentionPolicyCount: new(int64(0)),
		},
		TotalPolicyCount: new(encryption + network + data),
	}, err
}
