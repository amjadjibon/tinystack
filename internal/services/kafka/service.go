// Package kafka manages provisioned MSK configuration and optional Kafka-compatible broker containers.
package kafka

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/kafka"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kafka38              = "3.8.x"
	kafka39              = "3.9.x"
	serviceName          = "kafka"
	clusterKind          = "cluster"
	clientTLS            = "TLS"
	statusCreating       = "CREATING"
	statusActive         = "ACTIVE"
	staleRevisionMessage = "CurrentVersion does not match the stored configuration revision"
)

var _ api.API = (*Service)(nil)

// Network reads scoped EC2 metadata in the same transaction; it allocates nothing.
type Network interface {
	MountSubnet(storage.ReadTx, request.Scope, string) (*ec2api.Subnet, error)
	MountGroups(storage.ReadTx, request.Scope, string, []string) ([]string, error)
}

type Service struct {
	store       storage.Store
	network     Network
	runtime     Runtime
	logger      *slog.Logger
	bg          *background.Group
	owner       string
	mu          sync.Mutex // serializes the reconciler with Reset; API calls rely on transactions
	orphansDone bool
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

	r, ok, e := storage.Read[api.ClusterInfo](tx, scope.Key(serviceName, clusterKind, p[3]))
	if e != nil {
		return nil, e
	}

	if !ok || common.Value(r.ClusterArn) != arn {
		return nil, missing()
	}

	if common.Value(r.State) == "UPDATING" {
		deadline, found, err := storage.Read[time.Time](tx, transitionKey(scope, arn))
		if err != nil {
			return nil, err
		}

		if found && !time.Now().Before(*deadline) {
			r.State = new(statusActive)
		}
	}

	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *api.ClusterInfo) error {
	return storage.Write(tx, scope.Key(serviceName, clusterKind, *r.ClusterName), r)
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
		ClientAuthentication:      in.ClientAuthentication,
		EncryptionInfo:            in.EncryptionInfo,
	}

	if authErr := validateAuthentication(r); authErr != nil {
		return nil, authErr
	}

	if s.runtime != nil {
		if in.ConfigurationInfo != nil {
			return nil, invalid("ConfigurationInfo is unsupported in Docker mode")
		}

		if authErr := dockerAuthentication(r); authErr != nil {
			return nil, authErr
		}

		r.State = new(statusCreating)
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

		if in.ConfigurationInfo != nil {
			if err := applyConfiguration(tx, scope, r, in.ConfigurationInfo); err != nil {
				return err
			}
		}

		if err := s.validateNetwork(tx, scope, r.BrokerNodeGroupInfo); err != nil {
			return err
		}

		if s.runtime != nil {
			if err := storage.Write(tx, ownerKey, s.owner); err != nil {
				return err
			}

			if err := storage.Write(tx, runtimeKey(arn), runtimeRecord{ARN: arn}); err != nil {
				return err
			}
		}

		return save(tx, scope, r)
	})
	if e != nil {
		return nil, e
	}

	return common.Clone(
		&api.CreateClusterResponse{ClusterArn: r.ClusterArn, ClusterName: r.ClusterName, State: r.State},
	), nil
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

	if s.runtime != nil {
		e = s.store.View(ctx, func(tx storage.ReadTx) error {
			r, err := read(tx, scope, common.Value(in.ClusterArn))
			if err != nil {
				return err
			}

			if in.CurrentVersion != nil && *in.CurrentVersion != *r.CurrentVersion {
				return invalid(staleRevisionMessage)
			}

			return nil
		})
		if e != nil {
			return nil, e
		}

		if removeErr := s.runtime.Remove(ctx, resourceSpec(*in.ClusterArn, s.owner), true); removeErr != nil {
			return nil, removeErr
		}
	}

	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := read(tx, scope, common.Value(in.ClusterArn))
		if err != nil {
			return err
		}

		if in.CurrentVersion != nil && *in.CurrentVersion != *r.CurrentVersion {
			return invalid(staleRevisionMessage)
		}

		tx.Delete(scope.Key(serviceName, clusterKind, *r.ClusterName))
		tx.Delete(runtimeKey(*r.ClusterArn))
		tx.Delete(transitionKey(scope, *r.ClusterArn))
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

			stored, _, err := storage.Read[api.ClusterInfo](tx, key)
			if err != nil {
				return err
			}

			r, err := read(tx, request.Scope{AccountID: account, Region: region}, *stored.ClusterArn)
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
					Details: map[string]any{
						"RecordsOnly":    s.runtime == nil,
						"ExecutionReady": s.runtime != nil && common.Value(r.State) == statusActive,
						"Cluster":        common.Clone(r),
					},
				},
			)
		}

		return nil
	})
	return out, e
}
