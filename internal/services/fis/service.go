// Package fis stores experiment templates and executes only local aws:fis:wait actions.
package fis

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/fis"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName      = "fis"
	kindTemplate     = "experiment-template"
	kindExperiment   = "experiment"
	waitAction       = "aws:fis:wait"
	alarmSource      = "aws:cloudwatch:alarm"
	statusPending    = "pending"
	statusInitiating = "initiating"
	statusRunning    = "running"
	statusCompleted  = "completed"
	statusStopping   = "stopping"
	statusStopped    = "stopped"
	statusFailed     = "failed"
	statusCancelled  = "cancelled"
)

// Service stores regional FIS resources and owns their local execution worker.
type Service struct {
	store   storage.Store
	caller  *catalog.Caller
	workers *background.Group
}

// New constructs a service; UseCaller starts its worker once cross-service APIs are available.
func New(store storage.Store) *Service {
	return &Service{store: store, workers: background.New()}
}

// UseCaller installs cross-service lookups before starting the worker.
func (s *Service) UseCaller(c *catalog.Caller) {
	s.caller = c
	s.workers.Every(100*time.Millisecond, s.advance)
}

// Stop cancels and joins the worker before the storage backend closes.
func (s *Service) Stop() { s.workers.Stop() }

type record struct {
	Template   *api.ExperimentTemplate
	Experiment *api.Experiment
	Token      string
	Request    string
}

type experimentIndex struct {
	Scope request.Scope
	ID    string
}

func resourceARN(scope request.Scope, kind, id string) string {
	return fmt.Sprintf("arn:aws:fis:%s:%s:%s/%s", scope.Region, scope.AccountID, kind, id)
}

func missing() error {
	return awserror.New("ResourceNotFoundException", "FIS resource does not exist in this account and Region")
}

func readRecord(tx storage.ReadTx, scope request.Scope, kind, id string) (*record, error) {
	r, ok, err := storage.Read[record](tx, scope.Key(serviceName, kind, id))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing()
	}

	return r, nil
}

func (s *Service) get(ctx context.Context, kind string, id *string) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = readRecord(tx, scope, kind, common.Value(id))
		return e
	})
	return r, err
}

func (s *Service) list(ctx context.Context, kind string, token *string, limit *int64) ([]*record, *string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	var items []*record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		items, e = storage.Records[record](tx, scope.Prefix(serviceName, kind))
		return e
	})
	if err != nil {
		return nil, nil, err
	}

	return common.Page(items, func(r *record) string {
		if r.Template != nil {
			return common.Value(r.Template.Id)
		}

		return common.Value(r.Experiment.Id)
	}, token, int(common.Value(common.OrElse(limit, new(int64(100))))), awserror.Validation("Invalid nextToken"))
}

func parseARN(scope request.Scope, arn string) (kind, id string, err error) {
	prefix := fmt.Sprintf("arn:aws:fis:%s:%s:", scope.Region, scope.AccountID)
	if !strings.HasPrefix(arn, prefix) {
		return "", "", missing()
	}

	kind, id, ok := strings.Cut(strings.TrimPrefix(arn, prefix), "/")
	if !ok || id == "" || kind != kindTemplate && kind != kindExperiment {
		return "", "", missing()
	}

	return kind, id, nil
}

var _ api.API = (*Service)(nil)

func invalidToken() error { return awserror.Validation("Invalid nextToken") }
