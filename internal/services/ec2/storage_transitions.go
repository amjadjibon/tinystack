package ec2

import (
	"context"
	"log/slog"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const storageTransitionPrefix = "aws/ec2/index/"

type storageTransition struct {
	Scope request.Scope
	Kind  string
	ID    string
}

func storageTransitionKey(scope request.Scope, kind, id string) string {
	return storageTransitionPrefix + scope.AccountID + "/" + scope.Region + "/" + kind + "/" + id
}

func writePendingStorage(tx storage.WriteTx, scope request.Scope, kind, id string, record any) error {
	if err := storage.Write(tx, scope.Key("ec2", kind, id), record); err != nil {
		return err
	}

	return storage.Write(tx, storageTransitionKey(scope, kind, id), storageTransition{Scope: scope, Kind: kind, ID: id})
}

// StartStorageTransitions reconciles persisted EBS and AMI transitions in both execution modes.
func (s *Service) StartStorageTransitions() {
	s.storageBG = background.New()
	s.storageBG.Every(time.Second, s.visitStorageTransitions)
}

func (s *Service) visitStorageTransitions(ctx context.Context) {
	var keys []string
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
		keys = tx.Keys(storageTransitionPrefix)
		return nil
	}); err != nil {
		return
	}

	for _, key := range keys {
		err := s.store.Update(ctx, func(tx storage.WriteTx) error {
			job, found, err := storage.Read[storageTransition](tx, key)
			if err != nil || !found {
				return err
			}

			now := s.now().UTC()
			done, err := job.advance(tx, now)
			if err == nil && done {
				tx.Delete(key)
			}

			return err
		})
		if err != nil && ctx.Err() == nil {
			slog.Error("advance EC2 storage transition", "key", key, "error", err)
		}
	}
}

func (t *storageTransition) advance(tx storage.WriteTx, now time.Time) (bool, error) {
	key := t.Scope.Key("ec2", t.Kind, t.ID)
	switch t.Kind {
	case kindVolume:
		v, found, err := storage.Read[volume](tx, key)
		if err != nil || !found {
			return !found, err
		}

		done := v.advance(now)
		return done, storage.Write(tx, key, v)
	case kindSnapshot:
		snap, found, err := storage.Read[snapshot](tx, key)
		if err != nil || !found {
			return !found, err
		}

		snap.State = stateCompleted
		return true, storage.Write(tx, key, snap)
	case kindImage:
		img, found, err := storage.Read[registeredImage](tx, key)
		if err != nil || !found {
			return !found, err
		}

		for _, m := range img.Devices {
			id := common.Value(m.Ebs.SnapshotId)
			if id == "" {
				continue
			}

			snap, err := readAsset[snapshot](tx, t.Scope, kindSnapshot, id)
			if err != nil {
				return false, err
			}

			if snap != nil && snap.State == statePending {
				return false, nil
			}
		}

		img.State = stateAvailable
		return true, storage.Write(tx, key, img)
	default:
		return true, nil
	}
}

func (v *volume) advance(now time.Time) bool {
	if v.State == stateCreating {
		v.State = stateAvailable
	}

	m := v.Modification
	switch {
	case m == nil:
		return true
	case common.Value(m.ModificationState) == "modifying":
		v.volumeConfig = volumeConfig{
			Type: *m.TargetVolumeType, Size: *m.TargetSize, IOPS: *m.TargetIops, Throughput: *m.TargetThroughput,
		}

		m.ModificationState, m.Progress = new("optimizing"), new(int64(50))
		return false
	case common.Value(m.ModificationState) == "optimizing":
		m.ModificationState, m.Progress, m.EndTime = new(stateCompleted), new(int64(100)), new(now)
	}

	return true
}
