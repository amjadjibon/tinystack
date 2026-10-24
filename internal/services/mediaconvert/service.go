// Package mediaconvert stores MediaConvert configuration and runs a durable local job lifecycle without transcoding.
package mediaconvert

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/mediaconvert"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindJob           = "jobs"
	kindQueue         = "queues"
	kindPreset        = "presets"
	serviceName       = "mediaconvert"
	kindTemplate      = "jobTemplates"
	statusSubmitted   = "SUBMITTED"
	statusProgressing = "PROGRESSING"
	indexPrefix       = "aws/mediaconvert/index/"
)

// Service owns regional resources and persistent job steps.
type Service struct {
	store   storage.Store
	roles   *iam.Service
	caller  *catalog.Caller
	workers *background.Group
}

// New constructs a service; UseCaller starts background execution.
func New(store storage.Store, roles *iam.Service) *Service {
	return &Service{store: store, roles: roles, workers: background.New()}
}

// UseCaller connects S3 validation and EventBridge publishing.
func (s *Service) UseCaller(c *catalog.Caller) {
	s.caller = c
	s.workers.Every(200*time.Millisecond, s.advance)
}

// Stop joins the worker before storage closes.
func (s *Service) Stop() { s.workers.Stop() }

type record struct {
	Queue    *api.Queue
	Preset   *api.Preset
	Template *api.JobTemplate
	Job      *api.Job
	Tags     map[string]string
	Name     string
	Created  float64
	Sequence int64
}

type jobIndex struct {
	Scope request.Scope
	ID    string
}

func invalid(format string, args ...any) error {
	return awserror.New("BadRequestException", fmt.Sprintf(format, args...))
}

func missing() error {
	return awserror.New("NotFoundException", "MediaConvert resource does not exist")
}

func arn(scope request.Scope, kind, name string) string {
	return fmt.Sprintf("arn:aws:mediaconvert:%s:%s:%s/%s", scope.Region, scope.AccountID, kind, name)
}

func nameRef(scope request.Scope, kind, ref string) (string, error) {
	if !strings.HasPrefix(ref, "arn:") {
		return ref, nil
	}

	name, ok := strings.CutPrefix(ref, arn(scope, kind, ""))
	if !ok || name == "" {
		return "", missing()
	}

	return name, nil
}

func read(tx storage.ReadTx, scope request.Scope, kind, name string) (*record, error) {
	name, err := nameRef(scope, kind, name)
	if err != nil {
		return nil, err
	}

	r, ok, err := storage.Read[record](tx, scope.Key(serviceName, kind, name))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing()
	}

	return r, nil
}

func write(tx storage.WriteTx, scope request.Scope, kind string, r *record) error {
	if r.Sequence == 0 {
		key := scope.Key(serviceName, "sequence", kind)
		previous, _, err := storage.Read[int64](tx, key)
		if err != nil {
			return err
		}

		r.Sequence = 1
		if previous != nil {
			r.Sequence = *previous + 1
		}

		if writeErr := storage.Write(tx, key, r.Sequence); writeErr != nil {
			return writeErr
		}
	}

	if err := storage.Write(tx, scope.Key(serviceName, kind, r.Name), r); err != nil {
		return err
	}

	// Separate indexes preserve storage-key order for both AWS list modes.
	indexes := map[string]string{
		"NAME":          r.Name,
		"CREATION_DATE": fmt.Sprintf("%020.6f/%020d/%s", r.Created, r.Sequence, r.Name),
	}

	for mode, key := range indexes {
		if err := storage.Write(tx, scope.Key(serviceName, kind+"-"+mode, key), r.Name); err != nil {
			return err
		}
	}

	return nil
}

func remove(tx storage.WriteTx, scope request.Scope, kind string, r *record) {
	tx.Delete(scope.Key(serviceName, kind, r.Name))
	tx.Delete(scope.Key(serviceName, kind+"-NAME", r.Name))
	tx.Delete(
		scope.Key(serviceName, kind+"-CREATION_DATE", fmt.Sprintf("%020.6f/%020d/%s", r.Created, r.Sequence, r.Name)),
	)
}

func ensureDefault(tx storage.WriteTx, scope request.Scope) error {
	_, ok, err := storage.Read[record](tx, scope.Key(serviceName, kindQueue, "Default"))
	if err != nil || ok {
		return err
	}

	now := common.Unix(time.Now())
	return write(
		tx,
		scope,
		kindQueue,
		&record{Name: "Default", Created: now, Tags: map[string]string{}, Queue: &api.Queue{
			Name:        new("Default"),
			Arn:         new(arn(scope, kindQueue, "Default")),
			CreatedAt:   new(now),
			LastUpdated: new(now),
			Type:        new("SYSTEM"),
			Status:      new("ACTIVE"),
			PricingPlan: new("ON_DEMAND"),
		}},
	)
}

func (s *Service) get(ctx context.Context, kind string, name *string) (*record, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	var r *record
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		if err := ensureDefault(tx, scope); err != nil {
			return err
		}

		var err error
		r, err = read(tx, scope, kind, common.Value(name))
		if err != nil {
			return err
		}

		if r.Queue != nil {
			return queueCounts(tx, scope, r.Queue)
		}

		return nil
	})

	return r, err
}

func queueCounts(tx storage.ReadTx, scope request.Scope, q *api.Queue) error {
	jobs, err := storage.Records[record](tx, scope.Prefix(serviceName, kindJob))
	if err != nil {
		return err
	}

	submitted, progressing := int64(0), int64(0)
	for _, r := range jobs {
		if common.Value(r.Job.Queue) != common.Value(q.Arn) {
			continue
		}

		switch common.Value(r.Job.Status) {
		case statusSubmitted:
			submitted++
		case statusProgressing:
			progressing++
		}
	}

	q.SubmittedJobsCount = new(submitted)
	q.ProgressingJobsCount = new(progressing)
	return nil
}

var _ api.API = (*Service)(nil)
