package swf

import (
	"context"
	"fmt"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/swf"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// tick expires closed runs whose retention has ended, then advances every open run's deadlines: workflow and task
// timeouts and timers. Each run is one serializable transaction, written only when a deadline passed.
func (s *Service) tick(ctx context.Context) {
	changed := s.expire(ctx)

	var keys []string
	if err := s.store.View(
		ctx,
		func(tx storage.ReadTx) error { keys = tx.Keys(indexPrefix); return nil },
	); err != nil {
		return
	}

	for _, key := range keys {
		_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
			ref, ok, err := storage.Read[string](tx, key)
			if err != nil {
				return err
			}

			if !ok {
				return nil
			}

			r, ok, err := storage.Read[executionRecord](tx, *ref)
			if err != nil {
				return err
			}

			if !ok {
				tx.Delete(key)
				return nil
			}

			if !isOpen(r) {
				tx.Delete(key)
				return nil
			}

			t := now()
			dirty := false
			c := r.Detail.ExecutionConfiguration
			if expired(common.Value(r.Detail.ExecutionInfo.StartTimestamp), c.ExecutionStartToCloseTimeout, t) {
				event(
					r,
					&api.WorkflowExecutionTimedOutEventAttributes{
						TimeoutType: new(timeoutStartToClose),
						ChildPolicy: c.ChildPolicy,
					},
				)
				changed = true
				return closeExecution(tx, r, "TIMED_OUT", c.ChildPolicy)
			}

			if r.DecisionStarted != 0 && expired(r.DecisionStartedAt, c.TaskStartToCloseTimeout, t) {
				event(
					r,
					&api.DecisionTaskTimedOutEventAttributes{
						TimeoutType:      new(timeoutStartToClose),
						ScheduledEventId: new(r.DecisionScheduled),
						StartedEventId:   new(r.DecisionStarted),
					},
				)
				clearDecision(tx, r)
				r.Dirty = false
				scheduleDecision(r)
				dirty = true
			}

			for _, a := range orderedActivities(r) {
				kind := ""
				switch {
				case expired(a.ScheduledAt, a.Attributes.ScheduleToCloseTimeout, t):
					kind = "SCHEDULE_TO_CLOSE"
				case a.Started == 0 && expired(a.ScheduledAt, a.Attributes.ScheduleToStartTimeout, t):
					kind = "SCHEDULE_TO_START"
				case a.Started != 0 && expired(a.StartedAt, a.Attributes.StartToCloseTimeout, t):
					kind = timeoutStartToClose
				case a.Started != 0 && expired(a.HeartbeatAt, a.Attributes.HeartbeatTimeout, t):
					kind = "HEARTBEAT"
				}

				if kind != "" {
					event(
						r,
						&api.ActivityTaskTimedOutEventAttributes{
							TimeoutType:      new(kind),
							ScheduledEventId: new(a.Scheduled),
							StartedEventId:   new(a.Started),
							Details:          a.Details,
						},
					)
					finishActivity(tx, r, a)
					dirty = true
				}
			}

			// Visit timers in eventId order, including simultaneous expirations.
			for i := range r.History {
				e := &r.History[i]
				a := e.TimerStartedEventAttributes
				if a == nil {
					continue
				}

				timer := r.Timers[common.Value(a.TimerId)]
				if timer != nil && timer.Started == common.Value(e.EventId) && t >= timer.Due {
					event(r, &api.TimerFiredEventAttributes{TimerId: a.TimerId, StartedEventId: new(timer.Started)})
					delete(r.Timers, timer.ID)
					scheduleDecision(r)
					dirty = true
				}
			}

			if !dirty {
				return nil
			}

			changed = true
			return storage.Write(tx, r.Key, r)
		})
	}

	if changed {
		s.notify()
	}
}

// expire deletes the closed runs whose retention period has ended, reading the retention index in expiry order up
// to the first that has not, and reports whether it deleted any.
func (s *Service) expire(ctx context.Context) bool {
	deadline := fmt.Sprintf("%s%020d", retentionPrefix, time.Now().UnixNano())
	expired := false
	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, key := range tx.Keys(retentionPrefix) {
			if key > deadline {
				break
			}

			ref, ok, err := storage.Read[string](tx, key)
			if err != nil {
				return err
			}

			if ok {
				r, found, err := storage.Read[executionRecord](tx, *ref)
				if err != nil {
					return err
				}

				if found {
					tx.Delete(closedKey(r))
				}

				tx.Delete(*ref)
			}

			tx.Delete(key)
			expired = true
		}

		return nil
	})

	return expired
}
