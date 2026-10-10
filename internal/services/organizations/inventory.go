package organizations

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
			if !strings.HasSuffix(k, "/organizations/state") {
				continue
			}

			r, _, txErr := storage.Read[record](tx, k)
			if txErr != nil {
				return txErr
			}

			for _, x := range selected(r, "", "") {
				name := x.ID
				arn := resourceARN(r, x.Kind, x.ID)
				if x.Account != nil {
					name = common.Value(x.Account.Name)
				}

				if x.OU != nil {
					name = common.Value(x.OU.Name)
				}

				if x.Policy != nil {
					name = common.Value(x.Policy.PolicySummary.Name)
					arn = common.Value(x.Policy.PolicySummary.Arn)
				}

				out = append(
					out,
					catalog.Resource{
						Type:    x.Kind,
						Name:    x.ID,
						Account: common.Value(r.Organization.MasterAccountId),
						ARN:     arn,
						Tags:    x.Tags,
						Details: map[string]any{"Name": name, "ParentId": x.Parent},
					},
				)
			}
		}

		return nil
	})
	return out, err
}
