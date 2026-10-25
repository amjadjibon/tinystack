package bedrockagentcorecontrol

import (
	"context"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/storage"
)

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
			switch job.Kind {
			case kindRuntime:
				return settleRuntime(tx, job)
			case kindEndpoint:
				return settleEndpoint(tx, job)
			default:
				return settleMemory(tx, job)
			}
		})
	}
}

// settleRuntime finishes a runtime's creation or update, taking its latest version and its DEFAULT endpoint to
// READY, or removes a deleted runtime with its versions and endpoints.
func settleRuntime(tx storage.WriteTx, job *work) error {
	scope := job.Scope
	r, found, err := storage.Read[runtimeRecord](tx, runtimeKey(scope, job.ID))
	if err != nil {
		return err
	}

	if !found {
		tx.Delete(indexKey(scope, job.Kind, job.ID))

		return nil
	}

	now := time.Now().UTC()
	if now.Before(r.Due) {
		return nil
	}

	tx.Delete(indexKey(scope, job.Kind, job.ID))
	if r.Deleting {
		tx.Delete(runtimeKey(scope, r.ID))
		for _, prefix := range []string{versionPrefix(scope, r.ID), endpointPrefix(scope, r.ID)} {
			for _, key := range tx.Keys(prefix) {
				tx.Delete(key)
				if prefix == endpointPrefix(scope, r.ID) {
					tx.Delete(indexKey(scope, kindEndpoint, r.ID+"/"+strings.TrimPrefix(key, prefix)))
				}
			}
		}

		return nil
	}

	v, err := readVersion(tx, scope, r.ID, r.Latest)
	if err != nil {
		return err
	}

	v.Status, v.Updated = statusReady, now
	err = storage.Write(tx, versionKey(scope, r.ID, v.Number), v)
	if err != nil {
		return err
	}

	def, err := readEndpoint(tx, scope, r.ID, DefaultEndpoint)
	if err != nil {
		return err
	}

	def.Status, def.Live, def.Updated = statusReady, def.Target, now

	return storage.Write(tx, endpointKey(scope, r.ID, DefaultEndpoint), def)
}

func settleEndpoint(tx storage.WriteTx, job *work) error {
	scope := job.Scope
	runtimeID, name, _ := strings.Cut(job.ID, "/")
	ep, found, err := storage.Read[endpointRecord](tx, endpointKey(scope, runtimeID, name))
	if err != nil {
		return err
	}

	if !found {
		tx.Delete(indexKey(scope, job.Kind, job.ID))

		return nil
	}

	if time.Now().Before(ep.Due) {
		return nil
	}

	tx.Delete(indexKey(scope, job.Kind, job.ID))
	if ep.Status == statusDeleting {
		tx.Delete(endpointKey(scope, runtimeID, name))

		return nil
	}

	ep.Status, ep.Live, ep.Updated = statusReady, ep.Target, time.Now().UTC()

	return storage.Write(tx, endpointKey(scope, runtimeID, name), ep)
}

func settleMemory(tx storage.WriteTx, job *work) error {
	scope := job.Scope
	m, found, err := storage.Read[memoryRecord](tx, memoryKey(scope, job.ID))
	if err != nil {
		return err
	}

	if !found {
		tx.Delete(indexKey(scope, job.Kind, job.ID))

		return nil
	}

	if time.Now().Before(m.Due) {
		return nil
	}

	tx.Delete(indexKey(scope, job.Kind, job.ID))
	if m.Status == statusDeleting {
		tx.Delete(memoryKey(scope, m.ID))

		return nil
	}

	m.Status, m.Updated = statusActive, time.Now().UTC()

	return storage.Write(tx, memoryKey(scope, m.ID), m)
}
