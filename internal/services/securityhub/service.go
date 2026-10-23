// Package securityhub stores regional Security Hub records. Standards become READY
// immediately; findings are never evaluated or delivered to EventBridge.
package securityhub

import (
	"context"
	"fmt"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/securityhub"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName       = "securityhub"
	fieldResourceType = "ResourceType"
	fieldResourceID   = "ResourceId"
	fieldCreatedAt    = "CreatedAt"
	fieldUpdatedAt    = "UpdatedAt"
	comparisonPrefix  = "PREFIX"
)

type Service struct{ store storage.Store }

func New(store storage.Store) *Service { return &Service{store: store} }

type hubRecord struct {
	Hub  api.DescribeHubResponse
	Tags map[string]string
}

func invalid(message string) error { return awserror.New("InvalidInputException", message) }
func missing() error               { return awserror.New("ResourceNotFoundException", "Resource was not found.") }

func conflict() error { return awserror.New("ResourceConflictException", "Resource already exists.") }

func arn(scope request.Scope, resource string) string {
	return fmt.Sprintf("arn:aws:securityhub:%s:%s:%s", scope.Region, scope.AccountID, resource)
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// transaction checks enablement inside the same transaction as the operation.
func (s *Service) transaction(ctx context.Context, fn func(storage.WriteTx, request.Scope, *hubRecord) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		hub, found, err := storage.Read[hubRecord](tx, scope.Key(serviceName, "hub", "default"))
		if err != nil {
			return err
		}

		if !found {
			return awserror.New("InvalidAccessException", "Security Hub is not enabled for this account and Region.")
		}

		return fn(tx, scope, hub)
	})
}

func (s *Service) EnableSecurityHub(
	ctx context.Context,
	in *api.EnableSecurityHubRequest,
) (*api.EnableSecurityHubResponse, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	if err := tagging.CheckSet(in.Tags); err != nil {
		return nil, invalid(err.Error())
	}

	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := scope.Key(serviceName, "hub", "default")
		if _, ok := tx.Get(k); ok {
			return conflict()
		}

		h := hubRecord{
			Hub: api.DescribeHubResponse{
				HubArn:                  new(arn(scope, "hub/default")),
				SubscribedAt:            new(now()),
				AutoEnableControls:      new(true),
				ControlFindingGenerator: common.OrElse(in.ControlFindingGenerator, new("SECURITY_CONTROL")),
			},
			Tags: common.CloneTags(in.Tags),
		}

		if err := storage.Write(tx, k, h); err != nil {
			return err
		}

		if in.EnableDefaultStandards == nil || *in.EnableDefaultStandards {
			for _, standard := range standards(scope) {
				if common.Value(standard.EnabledByDefault) {
					if _, err := enableStandard(tx, scope, common.Value(standard.StandardsArn)); err != nil {
						return err
					}
				}
			}
		}

		return nil
	})

	return &api.EnableSecurityHubResponse{}, err
}

func (s *Service) DescribeHub(ctx context.Context, in *api.DescribeHubRequest) (*api.DescribeHubResponse, error) {
	var out api.DescribeHubResponse
	err := s.transaction(ctx, func(_ storage.WriteTx, _ request.Scope, h *hubRecord) error {
		if in.HubArn != nil && *in.HubArn != common.Value(h.Hub.HubArn) {
			return missing()
		}

		out = h.Hub
		return nil
	})

	return &out, err
}

func (s *Service) UpdateSecurityHubConfiguration(
	ctx context.Context,
	in *api.UpdateSecurityHubConfigurationRequest,
) (*api.UpdateSecurityHubConfigurationResponse, error) {
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, h *hubRecord) error {
		if in.AutoEnableControls != nil {
			h.Hub.AutoEnableControls = in.AutoEnableControls
		}

		if in.ControlFindingGenerator != nil {
			h.Hub.ControlFindingGenerator = in.ControlFindingGenerator
		}

		return storage.Write(tx, scope.Key(serviceName, "hub", "default"), h)
	})

	return &api.UpdateSecurityHubConfigurationResponse{}, err
}

func (s *Service) DisableSecurityHub(
	ctx context.Context,
	_ *api.DisableSecurityHubRequest,
) (*api.DisableSecurityHubResponse, error) {
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *hubRecord) error {
		for _, kind := range []string{"hub", "standard", "finding", "insight", "action"} {
			for _, k := range tx.Keys(scope.Prefix(serviceName, kind)) {
				tx.Delete(k)
			}
		}

		return nil
	})

	return &api.DisableSecurityHubResponse{}, err
}

var (
	_ api.API           = (*Service)(nil)
	_ catalog.Inventory = (*Service)(nil)
)
