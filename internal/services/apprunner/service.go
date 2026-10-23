// Package apprunner implements a records-only App Runner control plane.
// Mutations return OPERATION_IN_PROGRESS snapshots but atomically persist the terminal state;
// subsequent reads see RUNNING, PAUSED or DELETED. Operations are immediately SUCCEEDED.
// No source provider, container runtime, networking, build or deployment is executed.
package apprunner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apprunner"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindService              = "service"
	kindConnection           = "connection"
	kindVpcConnector         = "vpcconnector"
	kindAutoScaling          = "autoscalingconfiguration"
	kindObservability        = "observabilityconfiguration"
	statusActive             = "ACTIVE"
	statusInactive           = "INACTIVE"
	statusRunning            = "RUNNING"
	statusDeleted            = "DELETED"
	statusPaused             = "PAUSED"
	statusInProgress         = "OPERATION_IN_PROGRESS"
	defaultConfigurationName = "DefaultConfiguration"
)
const serviceName = "apprunner"

var _ api.API = (*Service)(nil)

type Service struct{ store storage.Store }

func New(store storage.Store) *Service { return &Service{store: store} }

type record struct {
	ARN                        string
	Name                       string
	Kind                       string
	Tags                       map[string]string
	Service                    *api.Service
	AutoScalingConfiguration   *api.AutoScalingConfiguration
	Connection                 *api.Connection
	VpcConnector               *api.VpcConnector
	ObservabilityConfiguration *api.ObservabilityConfiguration
	Operations                 []api.OperationSummary
	Revision                   int64
	Latest                     bool
}

func invalid(format string, args ...any) error {
	return awserror.New("InvalidRequestException", fmt.Sprintf(format, args...))
}

func missing() error {
	return awserror.New("ResourceNotFoundException", "Resource not found in this account and Region")
}

func resourceARN(scope request.Scope, kind, name string, revision int64) string {
	suffix := common.RandomID(32)
	if revision > 0 {
		return fmt.Sprintf(
			"arn:aws:apprunner:%s:%s:%s/%s/%d/%s",
			scope.Region,
			scope.AccountID,
			kind,
			name,
			revision,
			suffix,
		)
	}

	return fmt.Sprintf("arn:aws:apprunner:%s:%s:%s/%s/%s", scope.Region, scope.AccountID, kind, name, suffix)
}

func recordKey(scope request.Scope, arn string) (string, error) {
	prefix := fmt.Sprintf("arn:aws:apprunner:%s:%s:", scope.Region, scope.AccountID)
	if !strings.HasPrefix(arn, prefix) {
		return "", missing()
	}

	kind, id, ok := strings.Cut(strings.TrimPrefix(arn, prefix), "/")
	if !ok || id == "" {
		return "", invalid("Invalid App Runner ARN")
	}

	return scope.Key(serviceName, kind, id), nil
}

func readRecord(tx storage.ReadTx, scope request.Scope, arn, kind string) (*record, error) {
	k, err := recordKey(scope, arn)
	if err != nil {
		return nil, err
	}

	r, found, err := storage.Read[record](tx, k)
	if err != nil {
		return nil, err
	}

	if !found && (kind == kindAutoScaling || kind == kindObservability) {
		all, e := storage.Records[record](tx, scope.Prefix(serviceName, kind))
		if e != nil {
			return nil, e
		}

		for _, candidate := range all {
			if !strings.HasPrefix(candidate.ARN, arn+"/") {
				continue
			}

			active := candidate.AutoScalingConfiguration != nil &&
				common.Value(candidate.AutoScalingConfiguration.Status) == statusActive ||
				candidate.ObservabilityConfiguration != nil &&
					common.Value(candidate.ObservabilityConfiguration.Status) == statusActive
			if active && (r == nil || candidate.Revision > r.Revision) {
				r = candidate
				found = true
			}
		}
	}

	if !found || kind != "" && r.Kind != kind {
		return nil, missing()
	}

	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *record) error {
	k, err := recordKey(scope, r.ARN)
	if err != nil {
		return err
	}

	return storage.Write(tx, k, r)
}

func (s *Service) get(ctx context.Context, arn *string, kind string) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		r, readErr = readRecord(tx, scope, common.Value(arn), kind)
		if readErr != nil {
			return readErr
		}

		if r.Service != nil && r.Service.AutoScalingConfigurationSummary != nil {
			config, configErr := resolveScaling(
				tx,
				scope,
				r.Service.AutoScalingConfigurationSummary.AutoScalingConfigurationArn,
			)
			if configErr != nil {
				return configErr
			}

			r.Service.AutoScalingConfigurationSummary = scalingSummary(config)
		}

		return nil
	})
	return r, err
}

func (s *Service) list(
	ctx context.Context,
	kind string,
	name *string,
	latest *bool,
	maxResults *int64,
	token *string,
) ([]*record, *string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	limit := int(^uint(0) >> 1)
	if maxResults != nil {
		if *maxResults < 1 || *maxResults > 100 {
			return nil, nil, invalid("MaxResults must be between 1 and 100")
		}

		limit = int(*maxResults)
	}

	var items []*record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, e := storage.Records[record](tx, scope.Prefix(serviceName, kind))
		if e != nil {
			return e
		}

		for _, r := range all {
			if name != nil && r.Name != *name || common.Value(latest) && !r.Latest {
				continue
			}

			if r.Service != nil && common.Value(r.Service.Status) == statusDeleted ||
				name == nil && r.AutoScalingConfiguration != nil &&
					common.Value(r.AutoScalingConfiguration.Status) != statusActive ||
				r.VpcConnector != nil && common.Value(r.VpcConnector.Status) != statusActive ||
				name == nil && r.ObservabilityConfiguration != nil &&
					common.Value(r.ObservabilityConfiguration.Status) != statusActive {
				continue
			}

			items = append(items, r)
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return common.Page(items, func(r *record) string { return r.ARN }, token, limit, invalid("Invalid NextToken"))
}

func tagsMap(tags []api.Tag) (map[string]string, error) {
	result, err := tagging.Map(tags, func(t api.Tag) (*string, *string) { return t.Key, t.Value }, nil, nil)
	if err != nil {
		return nil, invalid("%s", err)
	}

	if err = tagging.CheckSet(result); err != nil {
		return nil, invalid("%s", err)
	}

	return result, nil
}

func (s *Service) insert(ctx context.Context, r *record) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := ensureDefault(tx, scope); e != nil {
			return e
		}

		all, e := storage.Records[record](tx, scope.Prefix(serviceName, r.Kind))
		if e != nil {
			return e
		}

		r.Revision = 1
		r.Latest = true
		for _, old := range all {
			if old.Service != nil && common.Value(old.Service.Status) == statusDeleted ||
				old.VpcConnector != nil && common.Value(old.VpcConnector.Status) == statusInactive {
				continue
			}

			if old.Name != r.Name {
				continue
			}

			if r.Kind == kindService || r.Kind == kindConnection || r.Kind == kindVpcConnector {
				return invalid("A resource with this name already exists")
			}

			if old.Revision >= r.Revision {
				r.Revision = old.Revision + 1
			}

			old.Latest = false
			if old.AutoScalingConfiguration != nil {
				old.AutoScalingConfiguration.Latest = new(false)
			}

			if old.ObservabilityConfiguration != nil {
				old.ObservabilityConfiguration.Latest = new(false)
			}

			if e := save(tx, scope, old); e != nil {
				return e
			}
		}

		rev := r.Revision
		if r.Kind == kindService || r.Kind == kindConnection {
			rev = 0
		}

		r.ARN = resourceARN(scope, r.Kind, r.Name, rev)
		switch r.Kind {
		case kindService:
			r.Service.ServiceArn = new(r.ARN)
			r.Service.ServiceId = new(r.ARN[strings.LastIndex(r.ARN, "/")+1:])
			r.Service.ServiceUrl = new(*r.Service.ServiceId + "." + scope.Region + ".awsapprunner.com")
		case kindConnection:
			r.Connection.ConnectionArn = new(r.ARN)
		case kindVpcConnector:
			r.VpcConnector.VpcConnectorArn = new(r.ARN)
		case kindAutoScaling:
			r.AutoScalingConfiguration.AutoScalingConfigurationArn = new(r.ARN)
			r.AutoScalingConfiguration.AutoScalingConfigurationRevision = new(r.Revision)
		case kindObservability:
			r.ObservabilityConfiguration.ObservabilityConfigurationArn = new(r.ARN)
			r.ObservabilityConfiguration.ObservabilityConfigurationRevision = new(r.Revision)
		}

		if r.Service != nil {
			config, configErr := resolveScaling(
				tx,
				scope,
				r.Service.AutoScalingConfigurationSummary.AutoScalingConfigurationArn,
			)
			if configErr != nil {
				return configErr
			}

			if common.Value(config.AutoScalingConfiguration.Status) != statusActive {
				return invalid("Configuration is inactive")
			}

			config.AutoScalingConfiguration.HasAssociatedService = new(true)
			r.Service.AutoScalingConfigurationSummary = scalingSummary(config)
			if e := save(tx, scope, config); e != nil {
				return e
			}

			operation(r, "CREATE_SERVICE")
		}

		return save(tx, scope, r)
	})
}

func operation(r *record, kind string) *string {
	id := common.RandomID(32)
	t := common.SecondsMilli(time.Now())
	r.Operations = append(
		r.Operations,
		api.OperationSummary{
			Id:        new(id),
			Type:      new(kind),
			Status:    new("SUCCEEDED"),
			TargetArn: new(r.ARN),
			StartedAt: t,
			EndedAt:   t,
			UpdatedAt: t,
		},
	)
	return new(id)
}

func (s *Service) changeService(
	ctx context.Context,
	arn *string,
	kind, status string,
	change func(*api.Service),
	scaling *string,
) (*api.Service, *string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	var out *api.Service
	var id *string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readRecord(tx, scope, common.Value(arn), kindService)
		if e != nil {
			return e
		}

		current := common.Value(r.Service.Status)
		if current == statusDeleted || kind == "RESUME_SERVICE" && current != statusPaused ||
			kind != "RESUME_SERVICE" && kind != "DELETE_SERVICE" && current != statusRunning {
			return awserror.New("InvalidStateException", "Invalid service state")
		}

		if scaling != nil {
			config, configErr := resolveScaling(tx, scope, scaling)
			if configErr != nil {
				return configErr
			}

			if common.Value(config.AutoScalingConfiguration.Status) != statusActive {
				return invalid("Configuration is inactive")
			}

			r.Service.AutoScalingConfigurationSummary = scalingSummary(config)
		}

		if change != nil {
			change(r.Service)
		}

		r.Service.Status = new(status)
		r.Service.UpdatedAt = common.SecondsMilli(time.Now())
		if status == statusDeleted {
			r.Service.DeletedAt = common.SecondsMilli(time.Now())
		}

		id = operation(r, kind)
		if e := save(tx, scope, r); e != nil {
			return e
		}

		if e := refreshAssociations(tx, scope); e != nil {
			return e
		}

		out = common.Clone(r.Service)
		out.Status = new(statusInProgress)
		return nil
	})
	return out, id, err
}
