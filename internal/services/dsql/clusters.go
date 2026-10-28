package dsql

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/dsql"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateCluster(ctx context.Context, in *api.CreateClusterInput) (*api.CreateClusterOutput, error) {
	return mutate(
		ctx,
		s,
		"CreateCluster",
		"",
		in.ClientToken,
		in,
		func(tx storage.WriteTx, scope request.Scope) (*api.CreateClusterOutput, error) {
			if in.KmsEncryptionKey != nil && *in.KmsEncryptionKey != "AWS_OWNED_KMS_KEY" {
				return nil, invalid("Custom KMS encryption keys are unsupported")
			}

			if err := checkPolicy(in.Policy, in.BypassPolicyLockoutSafetyCheck); err != nil {
				return nil, err
			}

			if err := tagging.CheckSet(in.Tags); err != nil {
				return nil, invalid(err.Error())
			}

			id := common.RandomID(26, "abcdefghijklmnopqrstuvwxyz0123456789")
			r := &record{GetClusterOutput: api.GetClusterOutput{
				Identifier:                new(id),
				Arn:                       new(resourceARN(scope, id)),
				Status:                    new(creating),
				CreationTime:              common.SecondsMilli(time.Now()),
				DeletionProtectionEnabled: common.OrElse(in.DeletionProtectionEnabled, new(true)),
				Tags:                      maps.Clone(in.Tags),
				Endpoint:                  new(fmt.Sprintf("%s.dsql.%s.on.aws", id, scope.Region)),
				EncryptionDetails: &api.EncryptionDetails{
					EncryptionType:   new("AWS_OWNED_KMS_KEY"),
					EncryptionStatus: new("ENABLED"),
				},
			}, Policy: in.Policy}
			if in.Policy != nil {
				r.PolicyVersion = common.RandomID(32)
			}

			if err := configurePeers(tx, scope, r, in.MultiRegionProperties); err != nil {
				return nil, err
			}

			status := creating
			if r.MultiRegionProperties != nil {
				status = pendingSetup
			}

			if err := queue(tx, scope, r, status); err != nil {
				return nil, err
			}

			return &api.CreateClusterOutput{
				Identifier: r.Identifier, Arn: r.Arn, Status: r.Status, CreationTime: r.CreationTime,
				MultiRegionProperties: r.MultiRegionProperties, EncryptionDetails: r.EncryptionDetails,
				DeletionProtectionEnabled: r.DeletionProtectionEnabled, Endpoint: r.Endpoint,
			}, nil
		},
	)
}

func (s *Service) GetCluster(ctx context.Context, in *api.GetClusterInput) (*api.GetClusterOutput, error) {
	r, err := s.get(ctx, in.Identifier)
	if err != nil {
		return nil, err
	}

	return &r.GetClusterOutput, nil
}

func (s *Service) ListClusters(ctx context.Context, in *api.ListClustersInput) (*api.ListClustersOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var items []*record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		items, e = storage.Records[record](tx, scope.Prefix(serviceName, kindCluster))
		return e
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(items, func(r *record) string { return common.Value(r.Identifier) },
		in.NextToken, int(common.Value(common.OrElse(in.MaxResults, new(int64(100))))), invalid("Invalid nextToken"))
	if err != nil {
		return nil, err
	}

	out := &api.ListClustersOutput{NextToken: next, Clusters: []api.ClusterSummary{}}
	for _, r := range page {
		out.Clusters = append(out.Clusters, api.ClusterSummary{Identifier: r.Identifier, Arn: r.Arn})
	}

	return out, nil
}

func (s *Service) UpdateCluster(ctx context.Context, in *api.UpdateClusterInput) (*api.UpdateClusterOutput, error) {
	return mutate(ctx, s, "UpdateCluster", common.Value(in.Identifier), in.ClientToken, in,
		func(tx storage.WriteTx, scope request.Scope) (*api.UpdateClusterOutput, error) {
			if in.KmsEncryptionKey != nil && *in.KmsEncryptionKey != "AWS_OWNED_KMS_KEY" {
				return nil, invalid("Custom KMS encryption keys are unsupported")
			}

			r, err := read(tx, scope, common.Value(in.Identifier))
			if err != nil {
				return nil, err
			}

			if common.Value(r.Status) != active && common.Value(r.Status) != pendingSetup {
				return nil, conflict("Cluster is not ACTIVE or PENDING_SETUP")
			}

			if s.runtime != nil && r.Activated && in.MultiRegionProperties != nil {
				if r.MultiRegionProperties == nil {
					return nil, invalid("Converting an active standalone Docker cluster to multi-Region is unsupported")
				}

				witness := in.MultiRegionProperties.WitnessRegion
				if witness != nil && *witness != common.Value(r.MultiRegionProperties.WitnessRegion) {
					return nil, invalid("Changing an active Docker cluster witness is unsupported")
				}

				if in.MultiRegionProperties.Clusters != nil {
					for _, arn := range in.MultiRegionProperties.Clusters {
						if !slices.Contains(r.MultiRegionProperties.Clusters, arn) {
							return nil, invalid("Changing active Docker cluster peers is unsupported")
						}
					}

					for _, arn := range r.MultiRegionProperties.Clusters {
						if arn != common.Value(r.Arn) && !slices.Contains(in.MultiRegionProperties.Clusters, arn) {
							return nil, invalid("Removing an active Docker cluster peer is unsupported")
						}
					}
				}
			}

			if err := configurePeers(tx, scope, r, in.MultiRegionProperties); err != nil {
				return nil, err
			}

			if in.DeletionProtectionEnabled != nil {
				r.DeletionProtectionEnabled = in.DeletionProtectionEnabled
			}

			if err := queue(tx, scope, r, updating); err != nil {
				return nil, err
			}

			return &api.UpdateClusterOutput{
				Identifier:   r.Identifier,
				Arn:          r.Arn,
				Status:       r.Status,
				CreationTime: r.CreationTime,
			}, nil
		})
}

func (s *Service) DeleteCluster(ctx context.Context, in *api.DeleteClusterInput) (*api.DeleteClusterOutput, error) {
	return mutate(ctx, s, "DeleteCluster", common.Value(in.Identifier), in.ClientToken, in,
		func(tx storage.WriteTx, scope request.Scope) (*api.DeleteClusterOutput, error) {
			r, err := read(tx, scope, common.Value(in.Identifier))
			if err != nil {
				return nil, err
			}

			if common.Value(r.DeletionProtectionEnabled) {
				return nil, conflict("Deletion protection is enabled")
			}

			status := common.Value(r.Status)
			if status != deleting && status != pendingDelete {
				if status != active && status != pendingSetup {
					return nil, conflict("Cluster is not ACTIVE or PENDING_SETUP")
				}

				status = deleting
				if r.MultiRegionProperties != nil && len(r.MultiRegionProperties.Clusters) > 1 {
					status = pendingDelete
				}

				if err := queue(tx, scope, r, status); err != nil {
					return nil, err
				}
			}

			return &api.DeleteClusterOutput{
				Identifier:   r.Identifier,
				Arn:          r.Arn,
				Status:       r.Status,
				CreationTime: r.CreationTime,
			}, nil
		})
}

func configurePeers(tx storage.ReadTx, scope request.Scope, r *record, in *api.MultiRegionProperties) error {
	if in == nil {
		return nil
	}

	p := common.Clone(in)
	if r.MultiRegionProperties != nil {
		if p.WitnessRegion == nil {
			p.WitnessRegion = r.MultiRegionProperties.WitnessRegion
		}

		if p.Clusters == nil {
			p.Clusters = slices.Clone(r.MultiRegionProperties.Clusters)
		}
	}

	witness := common.Value(p.WitnessRegion)
	if witness == "" || witness == scope.Region {
		return invalid("Witness Region must be specified and differ from cluster Region")
	}

	if regionSet(scope.Region) == "" || regionSet(scope.Region) != regionSet(witness) {
		return invalid("Cluster and witness must belong to the same supported Region set")
	}

	peers := []string{common.Value(r.Arn)}
	for _, arn := range p.Clusters {
		if arn == common.Value(r.Arn) {
			continue
		}

		if slices.Contains(peers, arn) {
			return invalid("Duplicate peer cluster")
		}

		remote, id, err := parseARN(scope, arn)
		if err != nil {
			return invalid("Peer must belong to the same account")
		}

		if remote.Region == scope.Region || remote.Region == witness {
			return invalid("Peer, local and witness Regions must differ")
		}

		if regionSet(remote.Region) != regionSet(scope.Region) {
			return invalid("Peer must belong to the same Region set")
		}

		peer, err := read(tx, remote, id)
		if err != nil {
			return err
		}

		if peer.MultiRegionProperties == nil || common.Value(peer.MultiRegionProperties.WitnessRegion) != witness {
			return invalid("Peer clusters must have the same witness Region")
		}

		if common.Value(peer.Status) == pendingDelete || common.Value(peer.Status) == deleting {
			return conflict("Peer is being deleted")
		}

		peers = append(peers, arn)
	}

	if len(peers) > 2 {
		return invalid("A multi-Region cluster has exactly two regional clusters")
	}

	p.Clusters = peers
	r.MultiRegionProperties = p
	return nil
}

// Region sets documented in the Aurora DSQL user guide; no cross-continent peering.
func regionSet(region string) string {
	switch region {
	case "us-east-1", "us-east-2", "us-west-2", "ca-central-1", "ca-west-1":
		return "americas"
	case "ap-east-1", "ap-south-1", "ap-northeast-1", "ap-northeast-2", "ap-northeast-3", "ap-southeast-1":
		return "asia"
	case "eu-central-1", "eu-west-1", "eu-west-2", "eu-west-3", "eu-south-2", "eu-north-1":
		return "europe"
	default:
		return ""
	}
}
