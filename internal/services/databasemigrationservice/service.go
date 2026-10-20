// Package databasemigrationservice stores DMS control-plane records. State transitions
// advance on Describe calls; no compute is provisioned and no data ever migrates.
package databasemigrationservice

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/databasemigrationservice"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName    = "databasemigrationservice"
	kindSubnet     = "subgrp"
	kindInstance   = "rep"
	kindEndpoint   = "endpoint"
	kindTask       = "task"
	stateReady     = "ready"
	stateAvailable = "available"
	stateCreating  = "creating"
	stateRunning   = "running"
	stateStarting  = "starting"
	stateStopped   = "stopped"
)

var (
	_                 api.API = (*Service)(nil)
	identifierPattern         = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]{0,254}$`)
)

type Service struct {
	store   storage.Store
	network *ec2.Service
}

func New(store storage.Store, network *ec2.Service) *Service {
	return &Service{store: store, network: network}
}

// record keeps tags alongside the resource. Passwords are accepted but never persisted:
// this records-only implementation has no connection execution that could consume them.
type record struct {
	Name     string
	ARN      string
	Tags     map[string]string
	Subnet   *api.ReplicationSubnetGroup
	Instance *api.ReplicationInstance
	Endpoint *api.Endpoint
	Task     *api.ReplicationTask
	Kind     string
}

func invalid(message string) error { return awserror.New("InvalidParameterValueException", message) }

func missing() error {
	return awserror.New("ResourceNotFoundFault", "The DMS resource does not exist.")
}

func stateError() error {
	return awserror.New("InvalidResourceStateFault", "The resource is not in a valid state.")
}

func checkIdentifier(id string) error {
	if !identifierPattern.MatchString(id) || strings.HasSuffix(id, "-") || strings.Contains(id, "--") {
		return invalid(
			"Identifiers must start with a letter, contain letters, digits and single hyphens, and end without a hyphen.",
		)
	}

	return nil
}

func resourceARN(scope request.Scope, kind, id string) string {
	return fmt.Sprintf("arn:aws:dms:%s:%s:%s:%s", scope.Region, scope.AccountID, kind, id)
}

func recordKey(scope request.Scope, r *record) string { return scope.Key(serviceName, r.Kind, r.ARN) }

func read(tx storage.ReadTx, scope request.Scope, kind, arn string) (*record, error) {
	r, found, err := storage.Read[record](tx, scope.Key(serviceName, kind, arn))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, missing()
	}

	return r, nil
}

func byName(tx storage.ReadTx, scope request.Scope, kind, name string) (*record, error) {
	rows, err := storage.Records[record](tx, scope.Prefix(serviceName, kind))
	if err != nil {
		return nil, err
	}

	for _, r := range rows {
		if r.Name == name {
			return r, nil
		}
	}

	return nil, missing()
}

func unique(tx storage.ReadTx, scope request.Scope, r *record) error {
	rows, err := storage.Records[record](tx, scope.Prefix(serviceName, r.Kind))
	if err != nil {
		return err
	}

	for _, other := range rows {
		if other.Name == r.Name && other.ARN != r.ARN {
			return awserror.New("ResourceAlreadyExistsFault", "A DMS resource with this identifier already exists.")
		}
	}

	return nil
}

func (s *Service) create(ctx context.Context, kind, name string, tags []api.Tag,
	build func(storage.WriteTx, request.Scope, *record) error,
) (*record, error) {
	if err := checkIdentifier(name); err != nil {
		return nil, err
	}

	tagMap, err := checkedTags(tags)
	if err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id := common.RandomID(26, common.AlphabetAlphanumeric)
	if kind == kindSubnet {
		id = name
	}

	r := &record{Name: name, Kind: kind, ARN: resourceARN(scope, kind, id), Tags: tagMap}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		// An exact ARN collision must also fail (subnet ARNs are identifier-based).
		if _, found := tx.Get(recordKey(scope, r)); found {
			return awserror.New("ResourceAlreadyExistsFault", "The DMS resource already exists.")
		}

		if e := unique(tx, scope, r); e != nil {
			return e
		}

		if e := build(tx, scope, r); e != nil {
			return e
		}

		return storage.Write(tx, recordKey(scope, r), r)
	})
	return r, err
}

func (s *Service) change(ctx context.Context, kind, arn string,
	edit func(storage.WriteTx, request.Scope, *record) error,
) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, kind, arn)
		if e != nil {
			return e
		}

		if e := edit(tx, scope, r); e != nil {
			return e
		}

		if e := checkIdentifier(r.Name); e != nil {
			return e
		}

		if e := unique(tx, scope, r); e != nil {
			return e
		}

		out = r
		return storage.Write(tx, recordKey(scope, r), r)
	})
	return out, err
}

func dependent(tx storage.ReadTx, scope request.Scope, arn string) error {
	rows, err := storage.Records[record](tx, scope.Prefix(serviceName, kindTask))
	if err != nil {
		return err
	}

	for _, r := range rows {
		t := r.Task
		if slices.Contains([]string{
			common.Value(t.SourceEndpointArn), common.Value(t.TargetEndpointArn),
			common.Value(t.ReplicationInstanceArn),
		}, arn) {
			return stateError()
		}
	}

	return nil
}

func (s *Service) remove(ctx context.Context, kind, arn string) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, kind, arn)
		if e != nil {
			return e
		}

		if e := dependent(tx, scope, arn); e != nil {
			return e
		}

		if r.Task != nil && !slices.Contains([]string{stateReady, stateStopped}, common.Value(r.Task.Status)) {
			return stateError()
		}

		if r.Instance != nil && common.Value(r.Instance.ReplicationInstanceStatus) != stateAvailable {
			return stateError()
		}

		if kind == kindSubnet {
			instances, e := storage.Records[record](tx, scope.Prefix(serviceName, kindInstance))
			if e != nil {
				return e
			}

			for _, i := range instances {
				if common.Value(i.Instance.ReplicationSubnetGroup.ReplicationSubnetGroupIdentifier) == r.Name {
					return stateError()
				}
			}
		}

		out = r
		tx.Delete(recordKey(scope, r))
		return nil
	})
	return out, err
}
