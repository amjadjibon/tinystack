package eventbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// scheduleIndex lists the enabled scheduled rules, which are all on the
// default bus, as "aws/eventbridge/schedule/<account>/<region>/<rule>" with
// their next fire time, so the scheduler reads no other rule.
const scheduleIndex = "aws/eventbridge/schedule/"

// scheduleInterval is how often the scheduler looks for due rules.
var scheduleInterval = time.Second

// scheduled is a schedule index entry.
type scheduled struct {
	// Next is the next fire time in Unix milliseconds.
	Next int64
}

func scheduleKey(scope request.Scope, name string) string {
	return scheduleIndex + scope.AccountID + "/" + scope.Region + "/" + name
}

// reschedule indexes the first fire time of an enabled scheduled rule set at
// now, and removes any other rule from the index.
func reschedule(tx storage.WriteTx, scope request.Scope, r *rule, now time.Time) error {
	key := scheduleKey(scope, r.Name)
	if r.Schedule == "" || r.State != stateEnabled {
		tx.Delete(key)
		return nil
	}

	s, err := parseSchedule(r.Schedule)
	if err != nil {
		return err
	}

	at, ok := s.first(now)
	if !ok {
		tx.Delete(key)
		return nil
	}

	return putScheduled(tx, key, at)
}

func putScheduled(tx storage.WriteTx, key string, at time.Time) error {
	data, err := json.Marshal(scheduled{Next: at.UnixMilli()})
	if err != nil {
		return fmt.Errorf("encode schedule: %w", err)
	}

	tx.Put(key, data)
	return nil
}

// run is one scheduled rule firing.
type run struct {
	at    time.Time
	scope request.Scope
	rule  compiledRule
}

// scheduler fires due rules every scheduleInterval until Stop.
func (s *Service) scheduler() {
	defer close(s.done)
	ticker := time.NewTicker(scheduleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}

		if err := s.fire(s.ctx, s.now()); err != nil && s.ctx.Err() == nil {
			s.logger.Warn("fire scheduled EventBridge rules", "error", err)
		}
	}
}

// due reports whether any scheduled rule is due at now.
func (s *Service) due(ctx context.Context, now time.Time) (bool, error) {
	found := false
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(scheduleIndex) {
			entry, err := loadScheduled(tx, k)
			if err != nil {
				return err
			}

			if entry.Next <= now.UnixMilli() {
				found = true
				return nil
			}
		}

		return nil
	})

	return found, err
}

func loadScheduled(tx storage.ReadTx, key string) (scheduled, error) {
	var entry scheduled
	data, _ := tx.Get(key)
	if err := json.Unmarshal(data, &entry); err != nil {
		return entry, fmt.Errorf("decode schedule %s: %w", key, err)
	}

	return entry, nil
}

// fire delivers a "Scheduled Event" for every rule due at now to its targets.
// Each rule's next fire time moves forward in the transaction that claims the
// run, before delivery, so a run is delivered at most once, even across a
// restart.
func (s *Service) fire(ctx context.Context, now time.Time) error {
	if ok, err := s.due(ctx, now); err != nil || !ok {
		return err
	}

	var runs []run
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		runs = nil
		for _, k := range tx.Keys(scheduleIndex) {
			entry, err := loadScheduled(tx, k)
			if err != nil {
				return err
			}

			if entry.Next > now.UnixMilli() {
				continue
			}

			r, ok, err := claim(tx, k, time.UnixMilli(entry.Next), now)
			if err != nil {
				return err
			}

			if ok {
				runs = append(runs, r)
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	for i := range runs {
		s.deliverScheduled(ctx, &runs[i])
	}

	return nil
}

// claim moves the index entry at key, due at due, to its next fire time and
// returns the run to deliver, if the rule still exists and has targets. An
// entry whose rule is gone, disabled, or no longer scheduled is removed.
func claim(tx storage.WriteTx, key string, due, now time.Time) (run, bool, error) {
	parts := strings.SplitN(strings.TrimPrefix(key, scheduleIndex), "/", 3)
	if len(parts) != 3 {
		tx.Delete(key)
		return run{}, false, nil
	}

	scope := request.Scope{AccountID: parts[0], Region: parts[1]}
	r, found, err := load(tx, rulePrefix(scope, defaultBus)+parts[2])
	if err != nil || !found || r.State != stateEnabled || r.Schedule == "" {
		tx.Delete(key)
		return run{}, false, err
	}

	// PutRule validated the expression.
	sched, err := parseSchedule(r.Schedule)
	if err != nil {
		return run{}, false, err
	}

	if next, ok := sched.after(due, now); ok {
		if putErr := putScheduled(tx, key, next); putErr != nil {
			return run{}, false, putErr
		}
	} else {
		tx.Delete(key)
	}

	return run{at: due, scope: scope, rule: compiledRule{
		name: r.Name, arn: ruleARN(scope, defaultBus, r.Name), targets: r.Targets,
	}}, len(r.Targets) > 0, nil
}

// deliverScheduled sends a run's event to its rule's targets, acting in the
// rule's account and region.
func (s *Service) deliverScheduled(ctx context.Context, r *run) {
	e := &event{
		Version:    "0",
		ID:         newID(),
		DetailType: "Scheduled Event",
		Source:     "aws.events",
		Account:    r.scope.AccountID,
		Time:       r.at.UTC().Format("2006-01-02T15:04:05Z"),
		Region:     r.scope.Region,
		Resources:  []string{r.rule.arn},
		Detail:     json.RawMessage("{}"),
	}

	encoded, err := json.Marshal(e)
	if err != nil {
		return
	}

	ctx = request.WithMetadata(ctx, request.Metadata{Scope: r.scope, RequestID: e.ID, Service: "events"})
	for i := range r.rule.targets {
		s.deliver(ctx, &r.rule, &r.rule.targets[i], e, encoded)
	}
}
