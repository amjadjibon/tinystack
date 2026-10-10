package rds

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources exposes database and group metadata without stored credentials.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok || !strings.Contains(rest, "/") {
				continue
			}

			r, found, e := storage.Read[record](tx, k)
			if e != nil {
				return e
			}

			if !found {
				continue
			}

			typ := map[string]string{kindDB: "db-instance", kindSubnet: subnetType, kindParameter: "db-parameter-group"}[r.Kind]
			details := map[string]any{}
			if r.Instance != nil {
				details["Engine"] = common.Value(r.Instance.Engine)
				details["Status"] = common.Value(r.Instance.DBInstanceStatus)
			}

			out = append(out, catalog.Resource{
				Type:    typ,
				Name:    r.Name,
				Account: account,
				Region:  region,
				ARN: arn(
					request.Scope{AccountID: account, Region: region},
					r.Kind,
					r.Name,
				),
				Tags:    r.Tags,
				Details: details,
			})
		}

		return nil
	})
	return out, err
}
