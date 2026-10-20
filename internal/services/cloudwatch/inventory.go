package cloudwatch

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists alarms, dashboards and metric streams, including delivery failures.
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
			case strings.HasPrefix(rest, kindStream+"/"):
				st, _, err := storage.Read[metricStream](tx, key)
				if err != nil {
					return err
				}
				out = append(out, catalog.Resource{
					Type: "metric-stream", Account: account, Region: region,
					Name: *st.Config.Name, ARN: *st.Config.Arn, Tags: st.Tags,
					Details: map[string]any{
						"State": *st.Config.State, "FirehoseArn": *st.Config.FirehoseArn,
						"Delivered": st.Delivered, "Failed": st.Failed, "LastError": st.LastError,
						"CreationDate": st.Config.CreationDate, "LastUpdateDate": st.Config.LastUpdateDate,
						"OutputFormat": st.Config.OutputFormat,
					},
				})
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
