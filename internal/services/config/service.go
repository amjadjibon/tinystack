// Package config implements regional AWS Config metadata. It does not capture
// resources, evaluate compliance, invoke Lambda, or deliver snapshots.
package config

import (
	"context"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/config"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName  = "config"
	kindRule     = "rule"
	kindRecorder = "recorder"
	kindChannel  = "channel"
	defaultName  = "default"
)

var _ api.API = (*Service)(nil)

// Keys: <account>/<region>/config/<kind>/<name>. Configuration and requested
// recorder state share one record and one transaction. There are no workers.
type record struct {
	Rule            *api.ConfigRule
	Recorder        *api.ConfigurationRecorder
	Channel         *api.DeliveryChannel
	Status          *api.ConfigurationRecorderStatus
	Tags            map[string]string
	Name, Kind, ARN string
}

type Service struct {
	store   storage.Store
	buckets *s3.Service
}

func New(store storage.Store, buckets *s3.Service) *Service {
	return &Service{store: store, buckets: buckets}
}

func invalid(message string) error       { return awserror.New("InvalidParameterValueException", message) }
func failure(code, message string) error { return awserror.New(code, message) }
func missing(kind string) error {
	switch kind {
	case kindRule:
		return failure("NoSuchConfigRuleException", "Config rule does not exist")
	case kindRecorder:
		return failure("NoSuchConfigurationRecorderException", "Configuration recorder does not exist")
	default:
		return failure("NoSuchDeliveryChannelException", "Delivery channel does not exist")
	}
}

func resourceARN(scope request.Scope, path string) string {
	return fmt.Sprintf("arn:aws:config:%s:%s:%s", scope.Region, scope.AccountID, path)
}

func read(tx storage.ReadTx, scope request.Scope, kind, name string) (*record, error) {
	r, found, err := storage.Read[record](tx, scope.Key(serviceName, kind, name))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, missing(kind)
	}
	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *record) error {
	return storage.Write(tx, scope.Key(serviceName, r.Kind, r.Name), r)
}

func (s *Service) edit(
	ctx context.Context,
	kind, name string,
	change func(storage.WriteTx, request.Scope, *record) error,
) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, kind, name)
		if e != nil {
			return e
		}
		if e := change(tx, scope, r); e != nil {
			return e
		}
		return save(tx, scope, r)
	})
}

func (s *Service) list(ctx context.Context, kind string, names []string) ([]*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	if kind != kindRule && len(names) > 1 {
		return nil, invalid("Only one recorder or delivery channel name is supported")
	}
	if len(names) > 25 {
		return nil, invalid("At most 25 rule names are supported")
	}
	selected := map[string]bool{}
	for _, name := range names {
		if name == "" || selected[name] {
			return nil, invalid("Names must be nonempty and unique")
		}
		selected[name] = true
	}
	out := []*record{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		// Validate requested names before returning any results.
		for _, name := range names {
			if _, e := read(tx, scope, kind, name); e != nil {
				return e
			}
		}
		for _, key := range tx.Keys(scope.Prefix(serviceName, kind)) {
			r, _, e := storage.Read[record](tx, key)
			if e != nil {
				return e
			}
			if len(selected) == 0 || selected[r.Name] {
				out = append(out, r)
			}
		}
		return nil
	})
	return out, err
}

func (s *Service) remove(ctx context.Context, kind, name string) error {
	if name == "" {
		return invalid("Resource name is required")
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, e := read(tx, scope, kind, name); e != nil {
			return e
		}
		if kind == kindChannel {
			for _, key := range tx.Keys(scope.Prefix(serviceName, kindRecorder)) {
				r, _, e := storage.Read[record](tx, key)
				if e != nil {
					return e
				}
				if common.Value(r.Status.Recording) {
					return failure(
						"LastDeliveryChannelDeleteFailedException",
						"Stop the recorder before deleting its delivery channel",
					)
				}
			}
		}
		tx.Delete(scope.Key(serviceName, kind, name))
		return nil
	})
}

func byARN(tx storage.ReadTx, scope request.Scope, arn string) (*record, error) {
	if arn == "" {
		return nil, invalid("ResourceArn is required")
	}
	if strings.HasPrefix(arn, resourceARN(scope, "")) {
		for _, kind := range []string{kindRule, kindRecorder} {
			for _, key := range tx.Keys(scope.Prefix(serviceName, kind)) {
				r, _, err := storage.Read[record](tx, key)
				if err != nil {
					return nil, err
				}
				if r.ARN == arn {
					return r, nil
				}
			}
		}
	}
	return nil, failure("ResourceNotFoundException", "Resource ARN does not exist in this account and Region")
}
