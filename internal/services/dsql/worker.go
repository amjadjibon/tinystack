package dsql

import (
	"context"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func peersReady(tx storage.ReadTx, scope request.Scope, r *record, deletion bool) bool {
	p := r.MultiRegionProperties
	if p == nil {
		return true
	}

	if !deletion && len(p.Clusters) != 2 {
		return false
	}

	for _, arn := range p.Clusters {
		if arn == common.Value(r.Arn) {
			continue
		}

		remote, id, err := parseARN(scope, arn)
		if err != nil {
			return false
		}

		peer, err := read(tx, remote, id)
		if err != nil {
			if deletion && awserror.From(err).Code == "ResourceNotFoundException" {
				continue
			}

			return false
		}

		status := common.Value(peer.Status)
		if deletion {
			if status != pendingDelete && status != deleting {
				return false
			}
		} else if peer.MultiRegionProperties == nil ||
			!slices.Contains(peer.MultiRegionProperties.Clusters, common.Value(r.Arn)) ||
			common.Value(peer.MultiRegionProperties.WitnessRegion) != common.Value(p.WitnessRegion) ||
			status == pendingDelete || status == deleting {
			return false
		}
	}

	return true
}

func (s *Service) advance(ctx context.Context) {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
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

		if s.runtime != nil && !s.prepareBackend(ctx, job) {
			continue
		}

		// Reread and claim each transition in the same transaction as its resulting state.
		_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
			r, err := read(tx, job.Scope, job.ID)
			if err != nil {
				return err
			}

			if time.Now().Before(r.Due) {
				return nil
			}

			status := common.Value(r.Status)
			switch status {
			case pendingSetup:
				if !peersReady(tx, job.Scope, r, false) {
					return nil
				}

				return queue(tx, job.Scope, r, creating)
			case creating, updating:
				if status == updating && !r.Activated && r.MultiRegionProperties != nil {
					return queue(tx, job.Scope, r, pendingSetup)
				}

				if !peersReady(tx, job.Scope, r, false) {
					return queue(tx, job.Scope, r, pendingSetup)
				}

				r.Status = new(active)
				r.Activated = true
			case pendingDelete:
				if !peersReady(tx, job.Scope, r, true) {
					return nil
				}

				return queue(tx, job.Scope, r, deleting)
			case deleting:
				tx.Delete(key(job.Scope, job.ID))
				tx.Delete(indexKey(job.Scope, job.ID))
				return nil
			}

			tx.Delete(indexKey(job.Scope, job.ID))
			return write(tx, job.Scope, r)
		})
	}
}
