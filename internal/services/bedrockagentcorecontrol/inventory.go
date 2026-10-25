package bedrockagentcorecontrol

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists runtimes, endpoints and memories.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok {
				continue
			}

			scope := request.Scope{AccountID: account, Region: region}
			switch {
			case strings.HasPrefix(rest, kindRuntime+"/"):
				r, _, err := storage.Read[runtimeRecord](tx, key)
				if err != nil {
					return err
				}

				v, err := readVersion(tx, scope, r.ID, r.Latest)
				if err != nil {
					return err
				}

				out = append(out, resource(scope, kindRuntime, r.ID, runtimeARN(scope, r.ID), r.Tags, map[string]any{
					"Name": r.Name, "Status": r.status(v), "LatestVersion": r.Latest, "Configuration": v.Config,
				}))
			case strings.HasPrefix(rest, kindEndpoint+"/"):
				ep, _, err := storage.Read[endpointRecord](tx, key)
				if err != nil {
					return err
				}

				id, _, _ := strings.Cut(strings.TrimPrefix(rest, kindEndpoint+"/"), "/")
				out = append(out, resource(scope, "runtime-endpoint", id+"/"+ep.Name, endpointARN(scope, id, ep.Name),
					ep.Tags, map[string]any{"Status": ep.Status, "LiveVersion": ep.Live, "TargetVersion": ep.Target}))
			case strings.HasPrefix(rest, kindMemory+"/"):
				m, _, err := storage.Read[memoryRecord](tx, key)
				if err != nil {
					return err
				}

				out = append(out, resource(scope, kindMemory, m.ID, memoryARN(scope, m.ID), m.Tags, m.output(scope)))
			}
		}

		return nil
	})

	return out, err
}

func resource(scope request.Scope, kind, name, arn string, tags map[string]string, config any) catalog.Resource {
	return catalog.Resource{
		Account: scope.AccountID, Region: scope.Region, Type: kind, Name: name, ARN: arn, Tags: maps.Clone(tags),
		Details: map[string]any{"Configuration": config},
	}
}
