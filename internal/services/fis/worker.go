package fis

import (
	"context"
	"fmt"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/fis"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func active(e *api.Experiment) bool {
	switch common.Value(e.State.Status) {
	case statusPending, statusInitiating, statusRunning, statusStopping:
		return true
	default:
		return false
	}
}

func (s *Service) advance(ctx context.Context) {
	var indices []*experimentIndex
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		indices, err = storage.Records[experimentIndex](tx, "aws/fis/index/")
		return err
	})
	if err != nil {
		return
	}

	for _, index := range indices {
		scoped := request.WithMetadata(ctx, request.Metadata{Scope: index.Scope})
		// Lookup alarms outside the transaction; the claimed step rereads current experiment state.
		r, err := s.get(scoped, kindExperiment, new(index.ID))
		if err != nil {
			continue
		}

		reason, failure := "", ""
		if active(r.Experiment) {
			for _, stop := range r.Experiment.StopConditions {
				if common.Value(stop.Source) != alarmSource {
					continue
				}

				state, err := s.alarmState(scoped, common.Value(stop.Value))
				if err != nil {
					failure = err.Error()
					break
				}

				if state == "ALARM" {
					reason = fmt.Sprintf("Stop condition triggered: %s is in ALARM state", common.Value(stop.Value))
					break
				}
			}
		}

		// The index remains until the terminal state commits, including after process restart.
		// Storage failures retain the index and are retried on the next poll.
		_ = s.store.Update(scoped, func(tx storage.WriteTx) error {
			current, err := readRecord(tx, index.Scope, kindExperiment, index.ID)
			if err != nil {
				return err
			}

			e := current.Experiment
			if !active(e) {
				tx.Delete(indexKey(index.Scope, index.ID))
				return nil
			}

			step(e, reason, failure, time.Now())
			if err := storage.Write(tx, index.Scope.Key(serviceName, kindExperiment, index.ID), current); err != nil {
				return err
			}

			if !active(e) {
				tx.Delete(indexKey(index.Scope, index.ID))
				return nil
			}

			return nil
		})
	}
}

func step(e *api.Experiment, stopReason, failure string, now time.Time) {
	stamp := common.SecondsMilli(now)
	status := common.Value(e.State.Status)
	if status != statusStopping {
		if failure != "" {
			e.State = &api.ExperimentState{Status: new(statusFailed), Reason: new(failure)}
			finishActions(e, statusFailed, stamp)
			e.EndTime = stamp
			return
		}

		if stopReason != "" {
			e.State = &api.ExperimentState{Status: new(statusStopping), Reason: new(stopReason)}
			return
		}
	}

	switch status {
	case statusPending:
		e.State.Status = new(statusInitiating)
	case statusInitiating:
		e.State.Status = new(statusRunning)
		e.StartTime = stamp
	case statusStopping:
		finishActions(e, statusStopped, stamp)
		e.State.Status = new(statusStopped)
		e.EndTime = stamp
	case statusRunning:
		// Read dependencies from the previous committed step so map iteration order cannot affect scheduling.
		ready := map[string]bool{}
		for name, a := range e.Actions {
			ready[name] = common.Value(a.State.Status) == statusCompleted
		}

		done := true
		for name, a := range e.Actions {
			switch common.Value(a.State.Status) {
			case statusPending:
				done = false
				canStart := true
				for _, dep := range a.StartAfter {
					canStart = canStart && ready[dep]
				}

				if canStart {
					a.State.Status = new(statusInitiating)
				}
			case statusInitiating:
				done = false
				a.State.Status = new(statusRunning)
				a.StartTime = stamp
			case statusRunning:
				duration, err := waitDuration(a.Parameters["duration"])
				if err != nil {
					a.State = &api.ExperimentActionState{Status: new(statusFailed), Reason: new(err.Error())}
					a.EndTime = stamp
					e.Actions[name] = a
					e.State = &api.ExperimentState{Status: new(statusFailed), Reason: new(err.Error())}
					finishActions(e, statusFailed, stamp)
					e.EndTime = stamp
					return
				}

				if common.Unix(now)-common.Value(a.StartTime) >= duration.Seconds() {
					a.State.Status = new(statusCompleted)
					a.EndTime = stamp
				} else {
					done = false
				}
			}

			e.Actions[name] = a
		}

		if done {
			e.State.Status = new(statusCompleted)
			e.EndTime = stamp
		}
	}
}

func finishActions(e *api.Experiment, runningStatus string, stamp *float64) {
	for name, a := range e.Actions {
		switch common.Value(a.State.Status) {
		case statusPending:
			a.State = &api.ExperimentActionState{Status: new(statusCancelled), Reason: e.State.Reason}
			a.EndTime = stamp
		case statusInitiating, statusRunning:
			a.State = &api.ExperimentActionState{Status: new(runningStatus), Reason: e.State.Reason}
			a.EndTime = stamp
		}

		e.Actions[name] = a
	}
}
