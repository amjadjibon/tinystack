// Package kafka stores provisioned MSK configuration records without running brokers.
package kafka

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/kafka"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName = "kafka"
	clusterKind = "cluster"
)

var _ api.API = (*Service)(nil)

// Network reads scoped EC2 metadata in the same transaction; it allocates nothing.
type Network interface {
	MountSubnet(storage.ReadTx, request.Scope, string) (*ec2api.Subnet, error)
	MountGroups(storage.ReadTx, request.Scope, string, []string) ([]string, error)
}
type Service struct {
	store   storage.Store
	network Network
}

func New(store storage.Store, network Network) *Service {
	return &Service{store: store, network: network}
}
func invalid(message string) error { return awserror.New("BadRequestException", message) }
func missing() error {
	e := awserror.New("NotFoundException", "Cluster does not exist in this account and Region")
	e.StatusCode = 404
	return e
}

func conflict(message string) error {
	e := awserror.New("ConflictException", message)
	e.StatusCode = 409
	return e
}
func revision() *string { return new("K" + strings.ReplaceAll(uuid.NewV4().String(), "-", "")) }

func read(tx storage.ReadTx, scope request.Scope, arn string) (*api.ClusterInfo, error) {
	p := arnPattern.FindStringSubmatch(arn)
	if p == nil {
		return nil, invalid("ClusterArn must be a commercial MSK cluster ARN, not an ID or name")
	}
	if p[1] != scope.Region || p[2] != scope.AccountID {
		return nil, missing()
	}
	r, ok, e := storage.Read[api.ClusterInfo](tx, scope.Key(serviceName, clusterKind, p[4]))
	if e != nil {
		return nil, e
	}
	if !ok || common.Value(r.ClusterArn) != arn {
		return nil, missing()
	}
	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *api.ClusterInfo) error {
	parts := strings.Split(*r.ClusterArn, "/")
	return storage.Write(tx, scope.Key(serviceName, clusterKind, parts[2]), r)
}

func (s *Service) CreateCluster(ctx context.Context, in *api.CreateClusterRequest) (*api.CreateClusterResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	in = common.Clone(in)
	if validationErr := validateCreate(in, scope); validationErr != nil {
		return nil, validationErr
	}
	arn := fmt.Sprintf(
		"arn:aws:kafka:%s:%s:cluster/%s/%s-1",
		scope.Region,
		scope.AccountID,
		*in.ClusterName,
		uuid.NewV4().String(),
	)
	r := &api.ClusterInfo{
		ClusterArn:                new(arn),
		ClusterName:               in.ClusterName,
		CreationTime:              new(time.Now().UTC()),
		CurrentVersion:            revision(),
		CurrentBrokerSoftwareInfo: &api.BrokerSoftwareInfo{KafkaVersion: in.KafkaVersion},
		BrokerNodeGroupInfo:       in.BrokerNodeGroupInfo,
		NumberOfBrokerNodes:       in.NumberOfBrokerNodes,
		EnhancedMonitoring:        in.EnhancedMonitoring,
		Tags:                      in.Tags,
	}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rows, err := storage.Records[api.ClusterInfo](tx, scope.Prefix(serviceName, clusterKind))
		if err != nil {
			return err
		}
		for _, other := range rows {
			if *other.ClusterName == *r.ClusterName {
				return conflict("ClusterName already exists")
			}
		}
		if err := s.validateNetwork(tx, scope, r.BrokerNodeGroupInfo); err != nil {
			return err
		}
		return save(tx, scope, r)
	})
	if e != nil {
		return nil, e
	}
	return common.Clone(&api.CreateClusterResponse{ClusterArn: r.ClusterArn, ClusterName: r.ClusterName}), nil
}

func (s *Service) DescribeCluster(
	ctx context.Context,
	in *api.DescribeClusterRequest,
) (*api.DescribeClusterResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	var r *api.ClusterInfo
	e = s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		r, err = read(tx, scope, common.Value(in.ClusterArn))
		return err
	})
	if e != nil {
		return nil, e
	}
	return &api.DescribeClusterResponse{ClusterInfo: common.Clone(r)}, nil
}

func (s *Service) DeleteCluster(ctx context.Context, in *api.DeleteClusterRequest) (*api.DeleteClusterResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := read(tx, scope, common.Value(in.ClusterArn))
		if err != nil {
			return err
		}
		if in.CurrentVersion != nil && *in.CurrentVersion != *r.CurrentVersion {
			return invalid("CurrentVersion does not match the stored configuration revision")
		}
		parts := strings.Split(*r.ClusterArn, "/")
		tx.Delete(scope.Key(serviceName, clusterKind, parts[2]))
		return nil
	})
	if e != nil {
		return nil, e
	}
	return &api.DeleteClusterResponse{ClusterArn: common.Clone(in.ClusterArn)}, nil
}

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	e := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok || !strings.HasPrefix(rest, clusterKind+"/") {
				continue
			}
			r, _, err := storage.Read[api.ClusterInfo](tx, key)
			if err != nil {
				return err
			}
			out = append(
				out,
				catalog.Resource{
					Type:    clusterKind,
					Name:    *r.ClusterName,
					ARN:     *r.ClusterArn,
					Account: account,
					Region:  region,
					Tags:    maps.Clone(r.Tags),
					Details: map[string]any{"RecordsOnly": true, "ExecutionReady": false, "Cluster": common.Clone(r)},
				},
			)
		}
		return nil
	})
	return out, e
}
