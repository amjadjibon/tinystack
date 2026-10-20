package databasemigrationservice

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

			r, _, e := storage.Read[record](tx, key)
			if e != nil {
				return e
			}

			var configuration any
			var state, kind string
			switch r.Kind {
			case kindSubnet:
				kind, state, configuration = "replication-subnet-group", common.Value(
					r.Subnet.SubnetGroupStatus,
				), r.Subnet
			case kindInstance:
				kind, state, configuration = "replication-instance", common.Value(
					r.Instance.ReplicationInstanceStatus,
				), r.Instance
			case kindEndpoint:
				kind, state, configuration = "endpoint", common.Value(r.Endpoint.Status), r.Endpoint
			case kindTask:
				kind, state, configuration = "replication-task", common.Value(r.Task.Status), r.Task
			default:
				continue
			}

			out = append(out, catalog.Resource{
				Account: account, Region: region, Type: kind, Name: r.Name, ARN: r.ARN,
				Tags: common.CloneTags(r.Tags), Details: map[string]any{
					"RecordsOnly": true, "ExecutionReady": false,
					"State": state, "Configuration": configuration,
				},
			})
		}

		return nil
	})
	return out, err
}
