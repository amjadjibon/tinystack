package sqs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Message move tasks are partition-wide records,
// "aws/sqs/move/<account>/<region>/<source queue>/<task ID>", so the mover
// finds running tasks without reading queues. Task IDs start with the start
// time, so a source's tasks list in start order.
const (
	moveIndex = "aws/sqs/move/"
	// defaultMoveRate is the rate of a task that sets none, messages per second.
	defaultMoveRate = 500
	// keptTasks is how many finished tasks a source queue keeps.
	keptTasks = 10

	taskRunning   = "RUNNING"
	taskCompleted = "COMPLETED"
	taskCancelled = "CANCELLED"
	taskFailed    = "FAILED"
)

// moveInterval is how often the mover moves each running task's next batch.
var moveInterval = 200 * time.Millisecond

type moveTask struct {
	ID             string
	SourceArn      string
	DestinationArn string `json:",omitempty"`
	Status         string
	FailureReason  string `json:",omitempty"`
	// Rate is the requested messages per second; zero means the default.
	Rate      int64 `json:",omitempty"`
	Moved     int64
	ToMove    int64
	StartedAt int64
}

func unsupported(message string) error { return awserror.New("UnsupportedOperation", message) }

func resourceNotFound(message string) error {
	return awserror.New("ResourceNotFoundException", message)
}

// taskPrefix is the prefix of the move task records of the queue stored at key.
func taskPrefix(key string) string {
	scope, name, _ := strings.Cut(key, "/sqs/")
	return moveIndex + scope + "/" + name + "/"
}

// handle is the opaque TaskHandle of a running task.
func (t *moveTask) handle() string {
	data, _ := json.Marshal(map[string]string{"taskId": t.ID, "sourceArn": t.SourceArn})
	return base64.RawURLEncoding.EncodeToString(data)
}

// parseHandle returns the record key of the task a TaskHandle names.
func parseHandle(handle string) (string, bool) {
	data, err := base64.RawURLEncoding.DecodeString(handle)
	if err != nil {
		return "", false
	}

	var h struct{ TaskID, SourceArn string }
	if json.Unmarshal(data, &h) != nil || h.TaskID == "" || strings.Contains(h.TaskID, "/") {
		return "", false
	}

	key, ok := arnKey(h.SourceArn)
	return taskPrefix(key) + h.TaskID, ok
}

// callerQueue maps an ARN to the key of a queue in the caller's account and
// region.
func callerQueue(ctx context.Context, name, arn string) (string, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return "", err
	}

	key, ok := arnKey(arn)
	if !ok {
		return "", invalid("Value %s for parameter %s is invalid. Reason: Must be an SQS queue ARN.", arn, name)
	}

	if !strings.HasPrefix(key, m.AccountID+"/"+m.Region+"/sqs/") {
		return "", resourceNotFound("The resource that you specified for the " + name + " parameter doesn't exist.")
	}

	return key, nil
}

// movable reports whether a task moves msg now: it must be visible and have
// somewhere to go, the task's destination or the queue it was moved from.
func movable(t *moveTask, msg *message, now int64) bool {
	return msg.VisibleAt <= now && (t.DestinationArn != "" || msg.SourceArn != "")
}

// StartMessageMoveTask starts moving the visible messages of a dead-letter
// queue to a destination queue, or back to the queues they came from. Only
// standard queues are supported, and a source has one running task at a time.
func (s *Service) StartMessageMoveTask(
	ctx context.Context,
	in *api.StartMessageMoveTaskRequest,
) (*api.StartMessageMoveTaskResult, error) {
	source, err := callerQueue(ctx, "SourceArn", common.Value(in.SourceArn))
	if err != nil {
		return nil, err
	}

	destination := ""
	if in.DestinationArn != nil {
		if destination, err = callerQueue(ctx, "DestinationArn", *in.DestinationArn); err != nil {
			return nil, err
		}

		if destination == source {
			return nil, invalid("The source and destination queues must be different.")
		}
	}

	rateErr := between("MaxNumberOfMessagesPerSecond", in.MaxNumberOfMessagesPerSecond, 1, defaultMoveRate)
	if rateErr != nil {
		return nil, rateErr
	}

	now := s.now().UnixMilli()
	t := &moveTask{
		ID:             fmt.Sprintf("%015d-%s", now, messageID()),
		SourceArn:      keyARN(source),
		DestinationArn: common.Value(in.DestinationArn),
		Status:         taskRunning,
		Rate:           common.Value(in.MaxNumberOfMessagesPerSecond),
		StartedAt:      now,
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		q, loadErr := load(tx, source)
		if loadErr != nil {
			return resourceNotFound("The resource that you specified for the SourceArn parameter doesn't exist.")
		}

		if q.Settings.FifoQueue {
			return unsupported("TinyStack supports message move tasks only for standard queues.")
		}

		sources, sourcesErr := deadLetterSources(tx, source)
		if sourcesErr != nil {
			return sourcesErr
		}

		if len(sources) == 0 {
			return invalid("Source queue must be configured as a Dead Letter Queue.")
		}

		if destination != "" {
			target, targetErr := load(tx, destination)
			if targetErr != nil {
				return resourceNotFound("The resource that you specified for the DestinationArn parameter " +
					"doesn't exist.")
			}

			if target.Settings.FifoQueue {
				return unsupported("TinyStack supports message move tasks only for standard queues.")
			}
		}

		prefix := taskPrefix(source)
		keys := tx.Keys(prefix)
		for _, k := range keys {
			running, _, taskErr := storage.Read[moveTask](tx, k)
			if taskErr != nil {
				return taskErr
			}

			if running.Status == taskRunning {
				return unsupported("There is already a task running. Only one active task is allowed for a " +
					"source queue at a given time.")
			}
		}

		// Keep the newest finished tasks.
		for len(keys) >= keptTasks {
			tx.Delete(keys[0])
			keys = keys[1:]
		}

		q.purge(now)
		for i := range q.Messages {
			if movable(t, &q.Messages[i], now) {
				t.ToMove++
			}
		}

		return storage.Write(tx, prefix+t.ID, t)
	})
	if err != nil {
		return nil, err
	}

	return &api.StartMessageMoveTaskResult{TaskHandle: new(t.handle())}, nil
}

// ListMessageMoveTasks lists a source queue's most recent tasks, newest first.
func (s *Service) ListMessageMoveTasks(
	ctx context.Context,
	in *api.ListMessageMoveTasksRequest,
) (*api.ListMessageMoveTasksResult, error) {
	source, err := callerQueue(ctx, "SourceArn", common.Value(in.SourceArn))
	if err != nil {
		return nil, err
	}

	limit := int64(1)
	if in.MaxResults != nil {
		if limit = *in.MaxResults; limit < 1 || limit > keptTasks {
			return nil, invalid("Value for parameter MaxResults is invalid. Reason: Must be between 1 and 10.")
		}
	}

	var tasks []*moveTask
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if _, loadErr := load(tx, source); loadErr != nil {
			return resourceNotFound("The resource that you specified for the SourceArn parameter doesn't exist.")
		}

		keys := tx.Keys(taskPrefix(source))
		for i := len(keys) - 1; i >= 0 && int64(len(tasks)) < limit; i-- {
			t, _, taskErr := storage.Read[moveTask](tx, keys[i])
			if taskErr != nil {
				return taskErr
			}

			tasks = append(tasks, t)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	out := &api.ListMessageMoveTasksResult{Results: []api.ListMessageMoveTasksResultEntry{}}
	for _, t := range tasks {
		entry := api.ListMessageMoveTasksResultEntry{
			Status:                            new(t.Status),
			SourceArn:                         new(t.SourceArn),
			ApproximateNumberOfMessagesMoved:  new(t.Moved),
			ApproximateNumberOfMessagesToMove: new(t.ToMove),
			StartedTimestamp:                  new(t.StartedAt),
		}
		if t.Status == taskRunning {
			entry.TaskHandle = new(t.handle())
		}

		if t.DestinationArn != "" {
			entry.DestinationArn = new(t.DestinationArn)
		}

		if t.Rate != 0 {
			entry.MaxNumberOfMessagesPerSecond = new(t.Rate)
		}

		if t.FailureReason != "" {
			entry.FailureReason = new(t.FailureReason)
		}

		out.Results = append(out.Results, entry)
	}

	return out, nil
}

// CancelMessageMoveTask stops a running task; messages already moved stay moved.
func (s *Service) CancelMessageMoveTask(
	ctx context.Context,
	in *api.CancelMessageMoveTaskRequest,
) (*api.CancelMessageMoveTaskResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	key, ok := parseHandle(common.Value(in.TaskHandle))
	if !ok || !strings.HasPrefix(key, moveIndex+m.AccountID+"/"+m.Region+"/") {
		return nil, resourceNotFound("Task does not exist.")
	}

	var moved int64
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, found, loadErr := storage.Read[moveTask](tx, key)
		switch {
		case loadErr != nil:
			return loadErr
		case !found:
			return resourceNotFound("Task does not exist.")
		case t.Status != taskRunning:
			return unsupported("Only a running task can be cancelled.")
		}

		t.Status, moved = taskCancelled, t.Moved
		return storage.Write(tx, key, t)
	})
	if err != nil {
		return nil, err
	}

	return &api.CancelMessageMoveTaskResult{ApproximateNumberOfMessagesMoved: new(moved)}, nil
}

// mover moves the next batch of every running task each moveInterval until Stop.
func (s *Service) mover(ctx context.Context) {
	var running []string
	_ = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(moveIndex) {
			if t, _, err := storage.Read[moveTask](tx, k); err == nil && t.Status == taskRunning {
				running = append(running, k)
			}
		}

		return nil
	})

	for _, k := range running {
		if err := s.move(ctx, k); err != nil {
			s.logger.Warn("move SQS messages", "task", k, "error", err)
		}
	}
}

// move moves a running task's next batch in one transaction: as many
// messages as its rate allows so far, oldest first, to the task's destination or to
// each message's source queue. The task completes when it has moved every
// message it counted, or none is left to move, and fails when its source or
// destination queue is gone. Messages whose source queue is gone stay.
func (s *Service) move(ctx context.Context, key string) error {
	now := s.now().UnixMilli()
	var notified []string
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, found, err := storage.Read[moveTask](tx, key)
		if err != nil || !found || t.Status != taskRunning {
			return err
		}

		source, _ := arnKey(t.SourceArn)
		q, err := load(tx, source)
		if err != nil {
			t.Status, t.FailureReason = taskFailed, "AWS.SimpleQueueService.NonExistentQueue"
			return storage.Write(tx, key, t)
		}

		rate := t.Rate
		if rate == 0 {
			rate = defaultMoveRate
		}

		// The task may have moved one message, plus its rate for every second
		// since it started.
		budget := 1 + rate*(now-t.StartedAt)/1000 - t.Moved
		targets := map[string]*queue{}
		q.purge(now)
		kept := q.Messages[:0]
		for i := range q.Messages {
			msg := q.Messages[i]
			if budget <= 0 || t.Moved >= t.ToMove || !movable(t, &msg, now) {
				kept = append(kept, msg)
				continue
			}

			arn := t.DestinationArn
			if arn == "" {
				arn = msg.SourceArn
			}

			target, exists, targetErr := moveTarget(tx, targets, arn)
			if targetErr != nil {
				return targetErr
			}

			if !exists && t.DestinationArn != "" {
				t.Status, t.FailureReason = taskFailed, "AWS.SimpleQueueService.NonExistentQueue"
				return storage.Write(tx, key, t)
			}

			if !exists {
				kept = append(kept, msg)
				continue
			}

			msg.SourceArn, msg.Receipt = "", ""
			msg.ReceiveCount, msg.FirstReceivedAt, msg.VisibleAt = 0, 0, now
			target.Messages = append(target.Messages, msg)
			t.Moved++
			budget--
		}

		q.Messages = kept
		for arn, target := range targets {
			if target == nil {
				continue
			}

			targetKey, _ := arnKey(arn)
			if saveErr := storage.Write(tx, targetKey, target); saveErr != nil {
				return saveErr
			}

			notified = append(notified, targetKey)
		}

		if t.Moved >= t.ToMove || !anyMovable(t, q, now) {
			t.Status = taskCompleted
		}

		if saveErr := storage.Write(tx, source, q); saveErr != nil {
			return saveErr
		}

		return storage.Write(tx, key, t)
	})
	if err != nil {
		return err
	}

	for _, k := range notified {
		s.notify(k)
	}

	return nil
}

// moveTarget loads the queue a message moves to once per batch, and reports
// false when that queue no longer exists (which targets records as nil).
func moveTarget(tx storage.ReadTx, targets map[string]*queue, arn string) (*queue, bool, error) {
	if target, ok := targets[arn]; ok {
		return target, target != nil, nil
	}

	targets[arn] = nil
	key, ok := arnKey(arn)
	if !ok {
		return nil, false, nil
	}

	if _, exists := tx.Get(key); !exists {
		return nil, false, nil
	}

	target, err := load(tx, key)
	if err != nil {
		return nil, false, err
	}

	targets[arn] = target
	return target, true, nil
}

func anyMovable(t *moveTask, q *queue, now int64) bool {
	for i := range q.Messages {
		if movable(t, &q.Messages[i], now) {
			return true
		}
	}

	return false
}
