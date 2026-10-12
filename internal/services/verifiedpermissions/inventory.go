package verifiedpermissions

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists the policy stores and policies across all accounts and regions.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok {
				continue
			}

			parts := strings.Split(rest, "/")
			if len(parts) == 2 && parts[0] == kindPolicyStore {
				id := parts[1]
				rec, found, err := storage.Read[policyStoreRecord](tx, key)
				if err != nil {
					return err
				}

				if !found || rec == nil {
					continue
				}

				out = append(out, catalog.Resource{
					Account: account,
					Region:  region,
					Type:    kindPolicyStore,
					Name:    id,
					ARN:     rec.Arn,
					Tags:    maps.Clone(rec.Tags),
					Details: map[string]any{
						"Description":        rec.Description,
						"ValidationSettings": rec.ValidationSettings,
					},
				})
			} else if len(parts) == 3 && parts[0] == kindPolicy {
				storeID := parts[1]
				policyID := parts[2]
				rec, found, err := storage.Read[policyRecord](tx, key)
				if err != nil {
					return err
				}

				if !found || rec == nil {
					continue
				}

				scope := request.Scope{AccountID: account, Region: region}
				out = append(out, catalog.Resource{
					Account: account,
					Region:  region,
					Type:    kindPolicy,
					Name:    policyID,
					ARN:     policyARN(scope, storeID, policyID),
					Tags:    nil,
					Details: map[string]any{
						"Description":   rec.Description,
						"Effect":        rec.Effect,
						"PolicyStoreId": rec.PolicyStoreId,
						"PolicyType":    rec.PolicyType,
					},
				})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
