package s3control

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok || !strings.HasPrefix(rest, "accesspoint/") {
				continue
			}

			r, _, err := storage.Read[accessPointRecord](tx, k)
			if err != nil {
				return err
			}

			out = append(out, catalog.Resource{Account: account, Region: region, Type: "accesspoint", Name: common.Value(r.Name), ARN: common.Value(r.AccessPointArn), Tags: common.CloneTags(r.Tags), Details: map[string]any{"Configuration": r.GetAccessPointResult}})
		}

		return nil
	})
	return out, err
}
