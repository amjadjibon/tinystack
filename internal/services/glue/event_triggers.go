package glue

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const triggerEvent = "EVENT"

type eventBatch struct {
	IDs      []string
	Deadline float64
}

func validateEventTrigger(t *api.Trigger) error {
	if common.Value(t.Type) != triggerEvent {
		if t.EventBatchingCondition != nil {
			return invalid("EventBatchingCondition requires an EVENT trigger")
		}

		return nil
	}

	if common.Value(t.WorkflowName) == "" {
		return invalid("EVENT triggers require WorkflowName")
	}

	if b := t.EventBatchingCondition; b != nil {
		if n := common.Value(b.BatchSize); n < 1 || n > 100 {
			return invalid("BatchSize must be between 1 and 100")
		}

		if b.BatchWindow != nil && (*b.BatchWindow < 1 || *b.BatchWindow > 900) {
			return invalid("BatchWindow must be between 1 and 900 seconds")
		}
	}

	return nil
}

func eventBatchLimits(t *api.Trigger) (size, window int64) {
	size, window = 1, 900
	if b := t.EventBatchingCondition; b != nil {
		size = *b.BatchSize
		window = *common.OrElse(b.BatchWindow, new(window))
	}

	return size, window
}

// DeliverWorkflowEvent receives a rule delivery already scoped and authorized by EventBridge.
func (s *Service) DeliverWorkflowEvent(ctx context.Context, arn, id string) error {
	return s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		prefix := "arn:aws:glue:" + scope.Region + ":" + scope.AccountID + ":workflow/"
		n, ok := strings.CutPrefix(arn, prefix)
		if !ok || n == "" || id == "" {
			return invalid("A local Glue workflow ARN and event ID are required")
		}

		if _, err := loadWorkflow(tx, scope, n); err != nil {
			return err
		}

		triggers, err := storage.Records[triggerRecord](tx, scope.Prefix(service, kindTrigger))
		if err != nil {
			return err
		}

		for _, r := range triggers {
			t := &r.Trigger
			if common.Value(t.WorkflowName) != n || common.Value(t.Type) != triggerEvent ||
				common.Value(t.State) != triggerActivated {
				continue
			}

			size, window := eventBatchLimits(t)
			now := common.Unix(s.now())
			last := len(r.EventBatches) - 1
			if last < 0 || r.EventBatches[last].Deadline <= now || int64(len(r.EventBatches[last].IDs)) >= size {
				r.EventBatches = append(r.EventBatches, eventBatch{Deadline: now + float64(window)})
				last++
			}

			r.EventBatches[last].IDs = append(r.EventBatches[last].IDs, id)
			return storage.Write(tx, orchestrationKey(scope, kindTrigger, *t.Name), r)
		}

		return nil
	})
}

// tickEventTrigger consumes a completed batch. Failed starts roll back their actions before the batch is removed.
func (s *Service) tickEventTrigger(ctx context.Context, ref *triggerReference) {
	var head eventBatch
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := loadTrigger(tx, ref.Scope, ref.Name)
		if err != nil {
			return err
		}

		if common.Value(r.Trigger.State) != triggerActivated || len(r.EventBatches) == 0 {
			return nil
		}

		head = r.EventBatches[0]
		size, window := eventBatchLimits(&r.Trigger)
		cause := &api.StartingEventBatchCondition{}
		switch {
		case int64(len(head.IDs)) >= size:
			cause.BatchSize = new(size)
		case head.Deadline <= common.Unix(s.now()):
			cause.BatchWindow = new(window)
		default:
			return nil
		}

		ids, err := json.Marshal(head.IDs)
		if err != nil {
			return err
		}

		input := &api.StartWorkflowRunRequest{
			Name: r.Trigger.WorkflowName, RunProperties: map[string]string{"aws:eventIds": string(ids)},
		}
		if _, err := s.startWorkflow(tx, ref.Scope, input, r, cause); err != nil {
			return err
		}

		r.EventBatches = r.EventBatches[1:]
		return storage.Write(tx, orchestrationKey(ref.Scope, kindTrigger, ref.Name), r)
	})
	var serviceErr *awserror.Error
	if !errors.As(err, &serviceErr) || len(head.IDs) == 0 {
		return
	}

	// Glue consumes the event batch even if the workflow cannot start, including when it is at its run limit.
	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := loadTrigger(tx, ref.Scope, ref.Name)
		if err != nil {
			return err
		}

		if len(r.EventBatches) > 0 && r.EventBatches[0].Deadline == head.Deadline &&
			r.EventBatches[0].IDs[0] == head.IDs[0] {
			r.EventBatches = r.EventBatches[1:]
			return storage.Write(tx, orchestrationKey(ref.Scope, kindTrigger, ref.Name), r)
		}

		return nil
	})
}
