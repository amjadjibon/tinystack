package s3files

import (
	"context"
	"time"

	"github.com/amjadjibon/tinystack/internal/storage"
)

// settler is the part of a record the worker advances: creating becomes available, deleting removes the record.
type settler interface {
	state() *string
	due() time.Time
}

func (r *fileSystem) state() *string  { return &r.State }
func (r *fileSystem) due() time.Time  { return r.Due }
func (r *mountTarget) state() *string { return &r.State }
func (r *mountTarget) due() time.Time { return r.Due }
func (r *accessPoint) state() *string { return &r.State }
func (r *accessPoint) due() time.Time { return r.Due }

// load reads the record at key, or returns nil when it is gone.
func load(tx storage.WriteTx, kind, key string) (settler, error) {
	switch kind {
	case kindFS:
		r, found, err := storage.Read[fileSystem](tx, key)
		if err != nil || !found {
			return nil, err
		}

		return r, nil
	case kindMT:
		r, found, err := storage.Read[mountTarget](tx, key)
		if err != nil || !found {
			return nil, err
		}

		return r, nil
	default:
		r, found, err := storage.Read[accessPoint](tx, key)
		if err != nil || !found {
			return nil, err
		}

		return r, nil
	}
}

// advance settles every indexed resource whose delay has passed. Each step rereads the record and claims it in
// the same transaction as the state it writes, so a deleted or already settled resource is left alone.
func (s *Service) advance(ctx context.Context) {
	var jobs []*work
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		jobs, err = storage.Records[work](tx, indexPrefix)
		return err
	}); err != nil {
		return
	}

	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}

		_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
			key := job.Scope.Key(serviceName, job.Kind, job.ID)
			r, err := load(tx, job.Kind, key)
			if err != nil {
				return err
			}

			if r == nil || (*r.state() != creating && *r.state() != deleting) {
				tx.Delete(indexKey(job.Scope, job.Kind, job.ID))
				return nil
			}

			if time.Now().Before(r.due()) {
				return nil
			}

			tx.Delete(indexKey(job.Scope, job.Kind, job.ID))
			if *r.state() == deleting {
				tx.Delete(key)
				return nil
			}

			*r.state() = available

			return storage.Write(tx, key, r)
		})
	}
}
