package backup

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, _, ok := storage.Scoped(key, serviceName)
			if !ok {
				continue
			}
			r, _, e := storage.Read[record](tx, key)
			if e != nil {
				return e
			}
			if r.Deleted != nil {
				continue
			}
			details := map[string]any{"RecordsOnly": true, "CreationDate": r.Created}
			if r.Kind == kindVault {
				details["EncryptionKeyArn"] = r.KeyARN
				details["NumberOfRecoveryPoints"] = 0
			} else {
				v := r.Versions[len(r.Versions)-1]
				details["BackupPlan"] = v.Plan
				details["VersionId"] = v.ID
				details["VersionCount"] = len(r.Versions)
				details["Selections"] = r.Selections
			}
			out = append(
				out,
				catalog.Resource{
					Type:    r.Kind,
					Name:    r.Name,
					ARN:     r.ARN,
					Account: account,
					Region:  region,
					Tags:    r.Tags,
					Details: details,
				},
			)
		}
		return nil
	})
	return out, err
}
