package ecs

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every active cluster, service, and task definition, for the
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
			r, ok, err := resource(tx, scope, key, rest)
			if err != nil {
				return err
			}

			if ok {
				r.Account, r.Region = account, region
				out = append(out, r)
			}
		}

		return nil
	})

	return out, err
}

// resource describes the record at key, if it is an active cluster, service,
// or task definition.
func resource(tx storage.ReadTx, scope request.Scope, key, rest string) (catalog.Resource, bool, error) {
	kind, _, _ := strings.Cut(rest, "/")
	switch kind {
	case kindCluster:
		c, _, err := read[cluster](tx, key)
		if err != nil || c.Status != active {
			return catalog.Resource{}, false, err
		}

		described, err := c.output(tx, scope, nil)
		if err != nil {
			return catalog.Resource{}, false, err
		}

		return catalog.Resource{
			Type: "cluster", Name: c.Name, ARN: *described.ClusterArn, Tags: tagMap(c.Tags),
			Details: map[string]any{
				"ActiveServices": *described.ActiveServicesCount, "RunningTasks": *described.RunningTasksCount,
			},
		}, true, nil
	case kindService:
		svc, _, err := read[ecsService](tx, key)
		if err != nil || svc.Status != active {
			return catalog.Resource{}, false, err
		}

		return catalog.Resource{
			Type: "service",
			Name: svc.Cluster + "/" + svc.Name,
			ARN:  serviceARN(scope, svc.Cluster, svc.Name),
			Tags: tagMap(
				svc.Tags,
			),
			Details: map[string]any{"TaskDefinition": svc.Definition, "DesiredCount": svc.Desired},
		}, true, nil
	case "taskdef":
		d, _, err := read[taskDef](tx, key)
		if err != nil || value(d.Def.Status) != active {
			return catalog.Resource{}, false, err
		}

		return catalog.Resource{
			Type: "task-definition", Name: *d.Def.Family, ARN: *d.Def.TaskDefinitionArn, Tags: tagMap(d.Tags),
			Details: map[string]any{"Revision": *d.Def.Revision, "Containers": len(d.Def.ContainerDefinitions)},
		}, true, nil
	}

	return catalog.Resource{}, false, nil
}
