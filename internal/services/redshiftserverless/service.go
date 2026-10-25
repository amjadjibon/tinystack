// Package redshiftserverless stores metadata; endpoints do not run a database.
package redshiftserverless

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/redshiftserverless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
	"github.com/amjadjibon/tinystack/internal/services/secretsmanager"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const serviceName = "redshift-serverless"

type Service struct {
	store   storage.Store
	network *ec2.Service
	secrets *secretsmanager.Service
	caller  *catalog.Caller
	workers *background.Group
}

type record struct {
	Due        time.Time
	Tags       map[string]string
	Namespace  *api.Namespace
	Workgroup  *api.Workgroup
	Snapshot   *api.Snapshot
	Settings   *api.Namespace
	UsageLimit *api.UsageLimit
	Endpoint   *api.EndpointAccess
	Name       string
	Kind       string
	ARN        string
}

type work struct {
	Scope request.Scope
	Kind  string
	Name  string
}

const indexPrefix = "aws/redshift-serverless/index/"

func New(store storage.Store, network *ec2.Service, secrets *secretsmanager.Service) *Service {
	s := &Service{store: store, network: network, secrets: secrets, workers: background.New()}

	s.workers.Every(100*time.Millisecond, s.advance)
	return s
}

func (s *Service) Stop() { s.workers.Stop() }

func (s *Service) UseCaller(c *catalog.Caller) { s.caller = c }

func invalid(msg string) error { return awserror.New("ValidationException", msg) }

// missing names the resource kind as AWS does; the Terraform provider recognizes a deleted snapshot by "snapshot" in
// the message.
func missing(kind, name string) error {
	return awserror.New("ResourceNotFoundException", fmt.Sprintf("%s %s not found", kind, name))
}

func conflict(msg string) error { return awserror.New("ConflictException", msg) }

func arn(scope request.Scope, kind, id string) string {
	return fmt.Sprintf("arn:aws:redshift-serverless:%s:%s:%s/%s", scope.Region, scope.AccountID, kind, id)
}

func read(tx storage.ReadTx, scope request.Scope, kind, name string) (*record, error) {
	r, ok, err := storage.Read[record](tx, scope.Key(serviceName, kind, name))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing(kind, name)
	}

	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *record) error {
	return storage.Write(tx, scope.Key(serviceName, r.Kind, r.Name), r)
}

func insert(tx storage.WriteTx, scope request.Scope, r *record) error {
	if _, ok := tx.Get(scope.Key(serviceName, r.Kind, r.Name)); ok {
		return conflict("Resource already exists")
	}

	return save(tx, scope, r)
}

func byARN(tx storage.ReadTx, scope request.Scope, value string) (*record, error) {
	for _, kind := range []string{kindEndpoint, kindNamespace, kindSnapshot, kindUsageLimit, kindWorkgroup} {
		rows, err := storage.Records[record](tx, scope.Prefix(serviceName, kind))
		if err != nil {
			return nil, err
		}

		for _, r := range rows {
			if r.ARN == value {
				return r, nil
			}
		}
	}

	return nil, missing("resource", value)
}

func (s *Service) get(ctx context.Context, kind string, name *string) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.View(
		ctx,
		func(tx storage.ReadTx) error { var e error; r, e = read(tx, scope, kind, common.Value(name)); return e },
	)
	return r, err
}

func (s *Service) list(
	ctx context.Context,
	kind string,
	token *string,
	maximum *int64,
	filter func(*record) bool,
) ([]*record, *string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	rows := []*record{}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, e := storage.Records[record](tx, scope.Prefix(serviceName, kind))
		if e != nil {
			return e
		}

		for _, r := range all {
			if filter == nil || filter(r) {
				rows = append(rows, r)
			}
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return common.Page(
		rows,
		func(r *record) string { return r.Name },
		token,
		int(common.Value(common.OrElse(maximum, new(int64(100))))),
		invalid("Invalid nextToken"),
	)
}

func queue(tx storage.WriteTx, scope request.Scope, r *record) error {
	r.Due = time.Now().UTC().Add(150 * time.Millisecond)
	if err := save(tx, scope, r); err != nil {
		return err
	}

	return storage.Write(
		tx,
		indexPrefix+scope.AccountID+"/"+scope.Region+"/"+r.Kind+"/"+r.Name,
		work{Scope: scope, Kind: r.Kind, Name: r.Name},
	)
}

func (s *Service) advance(ctx context.Context) {
	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, k := range tx.Keys(indexPrefix) {
			w, _, err := storage.Read[work](tx, k)
			if err != nil {
				return err
			}

			r, exists, err := storage.Read[record](tx, w.Scope.Key(serviceName, w.Kind, w.Name))
			if err != nil {
				return err
			}

			if !exists {
				tx.Delete(k)
				continue
			}

			if time.Now().Before(r.Due) {
				continue
			}

			if r.Workgroup != nil {
				r.Workgroup.Status = new(statusAvailable)
			}

			if r.Namespace != nil {
				r.Namespace.Status = new(statusAvailable)
			}

			if err := save(tx, w.Scope, r); err != nil {
				return err
			}

			tx.Delete(k)
		}

		return nil
	})
}

// LookupWorkgroup reads scoped metadata for Data API target validation.
func (s *Service) LookupWorkgroup(ctx context.Context, name string) (*api.Workgroup, error) {
	if !strings.HasPrefix(name, "arn:") {
		r, err := s.get(ctx, kindWorkgroup, new(name))
		if err != nil {
			return nil, err
		}

		return r.Workgroup, nil
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error { var e error; r, e = byARN(tx, scope, name); return e })
	if err != nil {
		return nil, err
	}

	if r.Workgroup == nil {
		return nil, missing(kindWorkgroup, name)
	}

	return r.Workgroup, nil
}

var _ api.API = (*Service)(nil)
