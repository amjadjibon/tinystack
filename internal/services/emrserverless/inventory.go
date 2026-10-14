package emrserverless

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/emrserverless"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok {
				continue
			}

			kind, _, _ := strings.Cut(rest, "/")
			var name, arn, state string
			var tags map[string]string
			var configuration any
			switch kind {
			case "application":
				a, _, e := storage.Read[api.Application](tx, k)
				if e != nil {
					return e
				}

				name = common.Value(a.Name)
				if name == "" {
					name = common.Value(a.ApplicationId)
				}

				arn, state, tags, configuration = common.Value(a.Arn), common.Value(a.State), a.Tags, a
			case "jobrun":
				j, _, e := storage.Read[api.JobRun](tx, k)
				if e != nil {
					return e
				}

				name = common.Value(j.Name)
				if name == "" {
					name = common.Value(j.JobRunId)
				}

				arn, state, tags, configuration = common.Value(j.Arn), common.Value(j.State), j.Tags, j
			default:
				continue
			}

			out = append(out, catalog.Resource{
				Account: account, Region: region, Type: kind, Name: name, ARN: arn,
				Tags: common.CloneTags(tags), Details: map[string]any{
					"RecordsOnly": true, "ExecutionReady": false,
					"State": state, "Configuration": configuration,
				},
			})
		}

		return nil
	})
	return out, err
}
