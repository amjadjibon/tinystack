package cloudwatch

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every alarm with its state and every dashboard, for the
// resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, service)
			if !ok {
				continue
			}

			scope := request.Scope{AccountID: account, Region: region}
			switch {
			case strings.HasPrefix(rest, kindAlarm+"/"):
				a, _, err := storage.Read[alarm](tx, key)
				if err != nil {
					return err
				}

				state, reason, _, _ := a.state()
				out = append(out, catalog.Resource{
					Type: "alarm", Account: account, Region: region, Name: a.name(), ARN: a.arn(), Tags: a.Tags,
					Details: map[string]any{"Type": a.kind(), "State": state, "Reason": reason},
				})
			case strings.HasPrefix(rest, kindDashboard+"/"):
				d, _, err := storage.Read[dashboard](tx, key)
				if err != nil {
					return err
				}

				out = append(out, catalog.Resource{
					Type: "dashboard", Account: account, Region: region, Name: d.Name,
					ARN: dashboardARN(scope, d.Name), Tags: d.Tags, Details: map[string]any{"Size": len(d.Body)},
				})
			}
		}

		return nil
	})

	return out, err
}
