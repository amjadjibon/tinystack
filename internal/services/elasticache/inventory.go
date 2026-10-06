package elasticache

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists all ElastiCache resources for the resource browser.
const (
	detailDescription = "Description"
	detailStatus      = "Status"
)

// Resources lists all ElastiCache resources for the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource

	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok {
				continue
			}

			scope := request.Scope{AccountID: account, Region: region}

			switch {
			case strings.HasPrefix(rest, "cluster/"):
				rec, found, err := storage.Read[clusterRecord](tx, k)
				if err != nil {
					return err
				}

				if !found {
					continue
				}

				out = append(out, catalog.Resource{
					Type:    "cache-cluster",
					Account: account,
					Region:  region,
					Name:    rec.ClusterID,
					ARN:     clusterARN(scope, rec.ClusterID),
					Tags:    rec.Tags,
					Details: map[string]any{
						"Engine":        rec.Engine,
						"EngineVersion": rec.EngineVersion,
						detailStatus:    rec.Status,
						"NumNodes":      rec.NumNodes,
						"Port":          rec.Port,
					},
				})

			case strings.HasPrefix(rest, "replicationgroup/"):
				rec, found, err := storage.Read[replicationGroupRecord](tx, k)
				if err != nil {
					return err
				}

				if !found {
					continue
				}

				out = append(out, catalog.Resource{
					Type:    "cache-replication-group",
					Account: account,
					Region:  region,
					Name:    rec.ReplicationGroupID,
					ARN:     replicationGroupARN(scope, rec.ReplicationGroupID),
					Tags:    rec.Tags,
					Details: map[string]any{
						detailDescription: rec.Description,
						detailStatus:      rec.Status,
						"NumClusters":     len(rec.MemberClusterIDs),
					},
				})

			case strings.HasPrefix(rest, "serverless/"):
				rec, found, err := storage.Read[serverlessCacheRecord](tx, k)
				if err != nil {
					return err
				}

				if !found {
					continue
				}

				out = append(out, catalog.Resource{
					Type:    "serverless-cache",
					Account: account,
					Region:  region,
					Name:    rec.ServerlessCacheName,
					ARN:     serverlessCacheARN(scope, rec.ServerlessCacheName),
					Tags:    rec.Tags,
					Details: map[string]any{
						"Engine":             rec.Engine,
						"MajorEngineVersion": rec.MajorEngineVersion,
						detailStatus:         rec.Status,
					},
				})

			case strings.HasPrefix(rest, "subnetgroup/") || strings.HasPrefix(rest, "subnet-group/"):
				rec, found, err := storage.Read[subnetGroupRecord](tx, k)
				if err != nil {
					return err
				}

				if !found {
					continue
				}

				out = append(out, catalog.Resource{
					Type:    "cache-subnet-group",
					Account: account,
					Region:  region,
					Name:    rec.Name,
					ARN:     subnetGroupARN(scope, rec.Name),
					Tags:    rec.Tags,
					Details: map[string]any{
						detailDescription: rec.Description,
						"VpcID":           rec.VpcID,
						"Subnets":         len(rec.Subnets),
					},
				})

			case strings.HasPrefix(rest, "parametergroup/") || strings.HasPrefix(rest, "parameter-group/"):
				rec, found, err := storage.Read[parameterGroupRecord](tx, k)
				if err != nil {
					return err
				}

				if !found {
					continue
				}

				out = append(out, catalog.Resource{
					Type:    "cache-parameter-group",
					Account: account,
					Region:  region,
					Name:    rec.Name,
					ARN:     parameterGroupARN(scope, rec.Name),
					Tags:    rec.Tags,
					Details: map[string]any{
						"Family":          rec.Family,
						detailDescription: rec.Description,
					},
				})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
