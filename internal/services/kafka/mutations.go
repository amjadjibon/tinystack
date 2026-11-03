package kafka

import (
	"context"
	"strings"
	"time"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/kafka"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// The completion deadline is durable, so transitions survive restart without a goroutine per update.
func transitionKey(scope request.Scope, arn string) string {
	return scope.Key(serviceName, "transition", arn)
}

func (s *Service) mutateCluster(
	ctx context.Context,
	arn, version *string,
	change func(storage.ReadTx, request.Scope, *api.ClusterInfo) error,
) (*string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if s.runtime != nil {
		return nil, invalid(
			"Cluster broker, storage, configuration and Kafka version updates are unsupported in Docker mode",
		)
	}

	operation := new(
		strings.Replace(common.Value(arn), ":cluster/", ":cluster-operation/", 1) + "/" + uuid.NewV4().String(),
	)

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, common.Value(arn))
		if e != nil {
			return e
		}

		if common.Value(version) == "" || common.Value(version) != common.Value(r.CurrentVersion) {
			return invalid(staleRevisionMessage)
		}

		if state := common.Value(r.State); state != "" && state != statusActive {
			return invalid("Cluster must be ACTIVE before updating")
		}

		if e := change(tx, scope, r); e != nil {
			return e
		}

		r.State = new("UPDATING")
		r.CurrentVersion = revision()
		if e := storage.Write(tx, transitionKey(scope, *arn), time.Now().UTC().Add(time.Second)); e != nil {
			return e
		}

		return save(tx, scope, r)
	})
	return operation, err
}

func (s *Service) UpdateBrokerCount(
	ctx context.Context,
	in *api.UpdateBrokerCountRequest,
) (*api.UpdateBrokerCountResponse, error) {
	op, err := s.mutateCluster(
		ctx,
		in.ClusterArn,
		in.CurrentVersion,
		func(_ storage.ReadTx, _ request.Scope, r *api.ClusterInfo) error {
			n := common.Value(in.TargetNumberOfBrokerNodes)
			if n <= common.Value(r.NumberOfBrokerNodes) || n > 15 ||
				n%int64(len(r.BrokerNodeGroupInfo.ClientSubnets)) != 0 {
				return invalid("Broker count must increase, be at most 15, and be a multiple of client subnet count")
			}

			r.NumberOfBrokerNodes = common.Clone(in.TargetNumberOfBrokerNodes)
			return nil
		},
	)
	return &api.UpdateBrokerCountResponse{ClusterArn: in.ClusterArn, ClusterOperationArn: op}, err
}

func (s *Service) UpdateBrokerStorage(
	ctx context.Context,
	in *api.UpdateBrokerStorageRequest,
) (*api.UpdateBrokerStorageResponse, error) {
	op, err := s.mutateCluster(
		ctx,
		in.ClusterArn,
		in.CurrentVersion,
		func(_ storage.ReadTx, _ request.Scope, r *api.ClusterInfo) error {
			if len(in.TargetBrokerEBSVolumeInfo) != 1 {
				return invalid("Specify exactly one storage update for ALL brokers")
			}

			b := in.TargetBrokerEBSVolumeInfo[0]
			n := common.Value(b.VolumeSizeGB)
			if !strings.EqualFold(common.Value(b.KafkaBrokerNodeId), "ALL") ||
				n <= common.Value(r.BrokerNodeGroupInfo.StorageInfo.EbsStorageInfo.VolumeSize) ||
				n <= 100 ||
				n > 16384 {
				return invalid("Storage must target ALL brokers, increase, and be 101-16384 GiB")
			}

			r.BrokerNodeGroupInfo.StorageInfo.EbsStorageInfo.VolumeSize = common.Clone(b.VolumeSizeGB)
			return nil
		},
	)
	return &api.UpdateBrokerStorageResponse{ClusterArn: in.ClusterArn, ClusterOperationArn: op}, err
}

func (s *Service) UpdateClusterConfiguration(
	ctx context.Context,
	in *api.UpdateClusterConfigurationRequest,
) (*api.UpdateClusterConfigurationResponse, error) {
	op, err := s.mutateCluster(
		ctx,
		in.ClusterArn,
		in.CurrentVersion,
		func(tx storage.ReadTx, scope request.Scope, r *api.ClusterInfo) error {
			return applyConfiguration(tx, scope, r, in.ConfigurationInfo)
		},
	)
	return &api.UpdateClusterConfigurationResponse{ClusterArn: in.ClusterArn, ClusterOperationArn: op}, err
}

func (s *Service) UpdateClusterKafkaVersion(
	ctx context.Context,
	in *api.UpdateClusterKafkaVersionRequest,
) (*api.UpdateClusterKafkaVersionResponse, error) {
	op, err := s.mutateCluster(
		ctx,
		in.ClusterArn,
		in.CurrentVersion,
		func(tx storage.ReadTx, scope request.Scope, r *api.ClusterInfo) error {
			if common.Value(r.CurrentBrokerSoftwareInfo.KafkaVersion) != kafka38 ||
				common.Value(in.TargetKafkaVersion) != kafka39 {
				return invalid("Only the 3.8.x to 3.9.x Kafka version upgrade is supported")
			}

			r.CurrentBrokerSoftwareInfo.KafkaVersion = common.Clone(in.TargetKafkaVersion)
			info := in.ConfigurationInfo
			if info == nil && r.CurrentBrokerSoftwareInfo.ConfigurationArn != nil {
				info = &api.ConfigurationInfo{
					Arn:      r.CurrentBrokerSoftwareInfo.ConfigurationArn,
					Revision: r.CurrentBrokerSoftwareInfo.ConfigurationRevision,
				}
			}

			if info != nil {
				return applyConfiguration(tx, scope, r, info)
			}

			return nil
		},
	)
	return &api.UpdateClusterKafkaVersionResponse{ClusterArn: in.ClusterArn, ClusterOperationArn: op}, err
}

func clusterV2(r *api.ClusterInfo) *api.Cluster {
	return &api.Cluster{
		ClusterType:    new("PROVISIONED"),
		ClusterArn:     r.ClusterArn,
		ClusterName:    r.ClusterName,
		CreationTime:   r.CreationTime,
		CurrentVersion: r.CurrentVersion,
		State:          r.State,
		StateInfo:      r.StateInfo,
		Tags:           r.Tags,
		Provisioned: &api.Provisioned{
			BrokerNodeGroupInfo:       r.BrokerNodeGroupInfo,
			CurrentBrokerSoftwareInfo: r.CurrentBrokerSoftwareInfo,
			EnhancedMonitoring:        r.EnhancedMonitoring,
			NumberOfBrokerNodes:       r.NumberOfBrokerNodes,
			ClientAuthentication:      r.ClientAuthentication,
			EncryptionInfo:            r.EncryptionInfo,
		},
	}
}

func (s *Service) DescribeClusterV2(
	ctx context.Context,
	in *api.DescribeClusterV2Request,
) (*api.DescribeClusterV2Response, error) {
	out, err := s.DescribeCluster(ctx, &api.DescribeClusterRequest{ClusterArn: in.ClusterArn})
	if err != nil {
		return nil, err
	}

	return &api.DescribeClusterV2Response{ClusterInfo: clusterV2(out.ClusterInfo)}, nil
}

func (s *Service) ListClustersV2(
	ctx context.Context,
	in *api.ListClustersV2Request,
) (*api.ListClustersV2Response, error) {
	if in.ClusterTypeFilter != nil && *in.ClusterTypeFilter != "PROVISIONED" && *in.ClusterTypeFilter != "SERVERLESS" {
		return nil, invalid("ClusterTypeFilter must be PROVISIONED or SERVERLESS")
	}

	if common.Value(in.ClusterTypeFilter) == "SERVERLESS" {
		if _, err := pageLimit(in.MaxResults); err != nil {
			return nil, err
		}

		if in.NextToken != nil {
			return nil, invalid("Invalid NextToken")
		}

		return &api.ListClustersV2Response{ClusterInfoList: []api.Cluster{}}, nil
	}

	out, err := s.listClusters(
		ctx,
		&api.ListClustersRequest{
			ClusterNameFilter: in.ClusterNameFilter,
			MaxResults:        in.MaxResults,
			NextToken:         in.NextToken,
		},
		"ListClustersV2/"+common.Value(in.ClusterTypeFilter),
	)
	if err != nil {
		return nil, err
	}

	rows := []api.Cluster{}
	for i := range out.ClusterInfoList {
		rows = append(rows, *clusterV2(&out.ClusterInfoList[i]))
	}

	return &api.ListClustersV2Response{ClusterInfoList: rows, NextToken: out.NextToken}, nil
}
