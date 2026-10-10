package config

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/common"
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
			r, _, err := storage.Read[record](tx, key)
			if err != nil {
				return err
			}
			details := map[string]any{"RecordsOnly": true}
			switch r.Kind {
			case kindRule:
				details["ConfigRule"] = r.Rule
			case kindRecorder:
				details["ConfigurationRecorder"] = r.Recorder
				details["RequestedRecording"] = common.Value(r.Status.Recording)
			case kindChannel:
				details["DeliveryChannel"] = r.Channel
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
