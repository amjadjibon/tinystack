// Package mwaa stores environment configuration without running Apache Airflow.
package mwaa

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
	api "github.com/amjadjibon/tinystack/internal/aws/generated/mwaa"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName     = "mwaa"
	environmentKind = "environment"
)

var _ api.API = (*Service)(nil)

// Network reads existing scoped EC2 metadata; it does not allocate networking.
type Network interface {
	MountSubnet(storage.ReadTx, request.Scope, string) (*ec2api.Subnet, error)
	MountGroups(storage.ReadTx, request.Scope, string, []string) ([]string, error)
}

// Source checks local S3 bucket metadata in the same transaction, without I/O.
type Source interface {
	CheckMWAASource(storage.ReadTx, request.Scope, string) error
}
type Service struct {
	store   storage.Store
	network Network
	source  Source
}
type record struct {
	Environment *api.Environment
	Revision    string
}

func New(store storage.Store, network Network, source Source) *Service {
	return &Service{store: store, network: network, source: source}
}
func invalid(message string) error { return awserror.New("ValidationException", message) }
func missing() error {
	e := awserror.New("ResourceNotFoundException", "Environment does not exist in this account and Region")
	e.StatusCode = 404
	return e
}
func revision() string { return uuid.NewV4().String() }
func read(tx storage.ReadTx, scope request.Scope, name string) (*record, error) {
	if !namePattern.MatchString(name) {
		return nil, invalid("Name requires 1-80 ASCII letters, digits, hyphens or underscores, starting with a letter")
	}
	r, ok, e := storage.Read[record](tx, scope.Key(serviceName, environmentKind, name))
	if e != nil {
		return nil, e
	}
	if !ok {
		return nil, missing()
	}
	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *record) error {
	return storage.Write(tx, scope.Key(serviceName, environmentKind, *r.Environment.Name), r)
}

func (s *Service) CreateEnvironment(
	ctx context.Context,
	in *api.CreateEnvironmentInput,
) (*api.CreateEnvironmentOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("CreateEnvironment input is required")
	}
	if in.Tags != nil && len(in.Tags) == 0 {
		return nil, invalid("Explicit Tags requires 1-50 pairs; omit for no tags")
	}
	name := common.Value(in.Name)
	in = common.Clone(in)
	in.Name = new(name)
	env := &api.Environment{
		Name:                 in.Name,
		ExecutionRoleArn:     in.ExecutionRoleArn,
		SourceBucketArn:      in.SourceBucketArn,
		DagS3Path:            in.DagS3Path,
		NetworkConfiguration: in.NetworkConfiguration,
		AirflowVersion:       in.AirflowVersion,
		EnvironmentClass:     in.EnvironmentClass,
		MinWorkers:           in.MinWorkers,
		MaxWorkers:           in.MaxWorkers,
		Schedulers:           in.Schedulers,
		Tags:                 in.Tags,
	}
	defaults(env)
	if configErr := validateConfig(env, scope); configErr != nil {
		return nil, configErr
	}
	env.Arn = new(fmt.Sprintf("arn:aws:airflow:%s:%s:environment/%s", scope.Region, scope.AccountID, *env.Name))
	env.CreatedAt = new(float64(time.Now().UTC().UnixMicro()) / 1e6)
	r := &record{Environment: env, Revision: revision()}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, ok := tx.Get(scope.Key(serviceName, environmentKind, *env.Name)); ok {
			return invalid("An environment with this Name already exists")
		}
		if err := s.validateReferences(tx, scope, env); err != nil {
			return err
		}
		return save(tx, scope, r)
	})
	if e != nil {
		return nil, e
	}
	return &api.CreateEnvironmentOutput{Arn: common.Clone(env.Arn)}, nil
}

func (s *Service) GetEnvironment(ctx context.Context, in *api.GetEnvironmentInput) (*api.GetEnvironmentOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	var r *record
	e = s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		r, err = read(tx, scope, common.Value(in.Name))
		return err
	})
	if e != nil {
		return nil, e
	}
	return &api.GetEnvironmentOutput{Environment: common.Clone(r.Environment)}, nil
}

func (s *Service) DeleteEnvironment(
	ctx context.Context,
	in *api.DeleteEnvironmentInput,
) (*api.DeleteEnvironmentOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := read(tx, scope, common.Value(in.Name))
		if err != nil {
			return err
		}
		tx.Delete(scope.Key(serviceName, environmentKind, *r.Environment.Name))
		return nil
	})
	if e != nil {
		return nil, e
	}
	return &api.DeleteEnvironmentOutput{}, nil
}

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	e := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok || !strings.HasPrefix(rest, environmentKind+"/") {
				continue
			}
			r, _, err := storage.Read[record](tx, key)
			if err != nil {
				return err
			}
			env := r.Environment
			out = append(out, catalog.Resource{
				Type:    environmentKind,
				Name:    *env.Name,
				ARN:     *env.Arn,
				Account: account,
				Region:  region,
				Tags: maps.Clone(
					env.Tags,
				),
				Details: map[string]any{"RecordsOnly": true, "ExecutionReady": false, "Environment": common.Clone(env)},
			})
		}
		return nil
	})
	return out, e
}
