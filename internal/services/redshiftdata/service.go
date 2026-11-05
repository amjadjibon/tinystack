// Package redshiftdata executes statement lifecycles using user fixtures, never a SQL engine.
package redshiftdata

import (
	"context"
	"fmt"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/redshiftdata"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/redshift"
	"github.com/amjadjibon/tinystack/internal/services/redshiftserverless"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName = "redshift-data"
	indexPrefix = "aws/redshift-data/index/"
)

type Service struct {
	store      storage.Store
	caller     *catalog.Caller
	clusters   *redshift.Service
	serverless *redshiftserverless.Service
	workers    *background.Group
}

type record struct {
	api.DescribeStatementResponse
	Name    *string
	Due     time.Time
	Expires time.Time
	SQLs    []string
	Answers []Fixture
	Batch   bool
}

type work struct {
	Scope request.Scope
	ID    string
}

func New(store storage.Store, clusters *redshift.Service, serverless *redshiftserverless.Service) *Service {
	s := &Service{store: store, clusters: clusters, serverless: serverless, workers: background.New()}

	s.workers.Every(100*time.Millisecond, s.advance)
	return s
}

func (s *Service) UseCaller(c *catalog.Caller) { s.caller = c }

func (s *Service) Stop() { s.workers.Stop() }

func invalid(message string) error { return awserror.New("ValidationException", message) }

func missing() error {
	return awserror.New("ResourceNotFoundException", "Statement does not exist or has expired")
}

func read(tx storage.ReadTx, scope request.Scope, id string) (*record, error) {
	r, ok, err := storage.Read[record](tx, scope.Key(serviceName, kindStatement, id))
	if err != nil {
		return nil, err
	}

	if !ok || !time.Now().Before(r.Expires) {
		return nil, missing()
	}

	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *record) error {
	return storage.Write(tx, scope.Key(serviceName, kindStatement, common.Value(r.Id)), r)
}

func (s *Service) get(ctx context.Context, id *string) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.View(
		ctx,
		func(tx storage.ReadTx) error { var e error; r, e = read(tx, scope, common.Value(id)); return e },
	)
	return r, err
}

func (s *Service) validateTarget(ctx context.Context, in *api.ExecuteStatementInput) error {
	if (in.ClusterIdentifier == nil) == (in.WorkgroupName == nil) {
		return invalid("Specify exactly one of ClusterIdentifier or WorkgroupName")
	}

	if in.ClusterIdentifier != nil {
		if _, err := s.clusters.LookupCluster(ctx, *in.ClusterIdentifier); err != nil {
			return invalid("ClusterIdentifier must identify an existing cluster")
		}

		if in.SecretArn == nil && in.DbUser == nil {
			return invalid("DbUser is required for cluster temporary credentials")
		}
	} else {
		name := common.Value(in.WorkgroupName)
		if _, err := s.serverless.LookupWorkgroup(ctx, name); err != nil {
			return invalid("WorkgroupName must identify an existing workgroup")
		}

		if in.DbUser != nil {
			return invalid("DbUser is not supported for serverless authentication")
		}
	}

	if in.SecretArn != nil {
		if in.DbUser != nil {
			return invalid("SecretArn conflicts with DbUser")
		}

		if _, err := s.caller.Call(
			ctx,
			"secretsmanager",
			"DescribeSecret",
			map[string]any{"SecretId": *in.SecretArn},
		); err != nil {
			return invalid("SecretArn must identify an existing secret")
		}
	}

	return nil
}

func (s *Service) advance(ctx context.Context) {
	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, k := range tx.Keys(indexPrefix) {
			w, _, err := storage.Read[work](tx, k)
			if err != nil {
				return err
			}

			r, exists, err := storage.Read[record](tx, w.Scope.Key(serviceName, kindStatement, w.ID))
			if err != nil {
				return err
			}

			if !exists || !time.Now().Before(r.Expires) {
				tx.Delete(w.Scope.Key(serviceName, kindStatement, w.ID))
				tx.Delete(k)
				continue
			}

			if time.Now().Before(r.Due) {
				continue
			}

			status := common.Value(r.Status)
			if status != statusSubmitted && status != statusStarted {
				continue
			}

			now := float64(time.Now().UTC().UnixMilli()) / 1000
			r.UpdatedAt = new(now)
			if status == statusSubmitted {
				r.Status = new(statusStarted)
				r.Due = time.Now().Add(150 * time.Millisecond)
			} else {
				finish(r)
			}

			if writeErr := save(tx, w.Scope, r); writeErr != nil {
				return writeErr
			}
		}

		return nil
	})
}

func finish(r *record) {
	r.Status = new(statusFinished)
	r.HasResultSet = new(false)
	r.ResultRows = new(int64(0))
	r.Duration = new(int64(0))
	r.ResultSize = new(int64(0))
	failed := -1
	for i, f := range r.Answers {
		if f.Error != nil {
			failed = i
			break
		}
	}

	for i, f := range r.Answers {
		status := statusFinished
		var message *string
		if failed >= 0 {
			status = statusAborted
			if i == failed {
				status = statusFailed
				message = f.Error
			}
		}

		rows := int64(len(f.Records))
		if f.NumberOfRecordsUpdated != nil {
			rows = *f.NumberOfRecordsUpdated
		}

		has := len(f.ColumnMetadata) > 0
		if r.Batch {
			r.SubStatements[i].Status = new(status)
			r.SubStatements[i].Error = message
			r.SubStatements[i].HasResultSet = new(has)
			r.SubStatements[i].ResultRows = new(rows)
			r.SubStatements[i].UpdatedAt = r.UpdatedAt
		}

		r.HasResultSet = new(common.Value(r.HasResultSet) || has)
		r.ResultRows = new(common.Value(r.ResultRows) + rows)
	}

	if failed >= 0 {
		r.Status = new(statusFailed)
		r.Error = r.Answers[failed].Error
		r.HasResultSet = new(false)
		r.ResultRows = new(int64(-1))
	}
}

func subID(id string, index int) string { return fmt.Sprintf("%s:%d", id, index+1) }

var _ api.API = (*Service)(nil)
