package swf

import (
	"context"
	"strconv"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/swf"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type tokenRecord struct {
	Key        string
	ActivityID string
}

type decisionPage struct {
	After    *string
	Key      string
	Token    string
	Previous int64
	Start    int64
	Reverse  bool
}

func tokenKey(scope request.Scope, token string) string {
	return scope.Key(serviceName, "token", token)
}

func readToken(
	tx storage.ReadTx,
	scope request.Scope,
	token *string,
	decision bool,
) (*executionRecord, *activity, error) {
	ref, ok, err := storage.Read[tokenRecord](tx, tokenKey(scope, common.Value(token)))
	if err != nil {
		return nil, nil, err
	}

	if !ok || (ref.ActivityID == "") != decision {
		return nil, nil, missing()
	}

	r, ok, err := storage.Read[executionRecord](tx, ref.Key)
	if err != nil {
		return nil, nil, err
	}

	if !ok || !isOpen(r) {
		return nil, nil, missing()
	}

	if decision {
		if r.DecisionToken != common.Value(token) || r.DecisionStarted == 0 {
			return nil, nil, missing()
		}

		return r, nil, nil
	}

	a, ok := r.Activities[ref.ActivityID]
	if !ok || a.Token != common.Value(token) || a.Started == 0 {
		return nil, nil, missing()
	}

	return r, a, nil
}

func (s *Service) longPoll(ctx context.Context, claim func() (bool, error)) error {
	timer := time.NewTimer(60 * time.Second)
	defer timer.Stop()
	for {
		changed := s.watch()
		found, err := claim()
		if err != nil || found {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.workers.Context().Done():
			return nil
		case <-timer.C:
			return nil
		case <-changed:
		}
	}
}

func taskPriority(p *string) int64 { n, _ := strconv.ParseInt(common.Value(p), 10, 32); return n }
func orderedActivities(r *executionRecord) []*activity {
	out := []*activity{}
	for i := range r.History {
		e := &r.History[i]
		if a := e.ActivityTaskScheduledEventAttributes; a != nil {
			if current := r.Activities[common.Value(a.ActivityId)]; current != nil &&
				current.Scheduled == common.Value(e.EventId) {
				out = append(out, current)
			}
		}
	}

	return out
}

func (s *Service) PollForActivityTask(
	ctx context.Context,
	in *api.PollForActivityTaskInput,
) (*api.ActivityTask, error) {
	out := &api.ActivityTask{TaskToken: new("")}
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	err := s.longPoll(ctx, func() (bool, error) {
		found := false
		err := s.store.Update(ctx, func(tx storage.WriteTx) error {
			if _, err := readDomain(tx, scope, common.Value(in.Domain), false); err != nil {
				return err
			}

			rs, err := storage.Records[executionRecord](tx, executionPrefix(scope, common.Value(in.Domain)))
			if err != nil {
				return err
			}

			var selected *executionRecord
			var task *activity
			for _, r := range rs {
				if !isOpen(r) {
					continue
				}

				for _, a := range orderedActivities(r) {
					if a.Started != 0 || common.Value(a.Attributes.TaskList.Name) != common.Value(in.TaskList.Name) {
						continue
					}

					if task == nil ||
						taskPriority(a.Attributes.TaskPriority) > taskPriority(task.Attributes.TaskPriority) ||
						(taskPriority(a.Attributes.TaskPriority) == taskPriority(task.Attributes.TaskPriority) &&
							a.ScheduledAt < task.ScheduledAt) {
						selected = r
						task = a
					}
				}
			}

			if task == nil {
				return nil
			}

			task.Token = common.RandomID(32, "0123456789abcdef")
			task.Started = event(
				selected,
				&api.ActivityTaskStartedEventAttributes{ScheduledEventId: new(task.Scheduled), Identity: in.Identity},
			)
			task.StartedAt = now()
			task.HeartbeatAt = task.StartedAt
			selected.Detail.LatestActivityTaskTimestamp = new(task.StartedAt)
			out = &api.ActivityTask{
				TaskToken:         new(task.Token),
				ActivityId:        task.Attributes.ActivityId,
				StartedEventId:    new(task.Started),
				WorkflowExecution: selected.Detail.ExecutionInfo.Execution,
				ActivityType:      task.Attributes.ActivityType,
				Input:             task.Attributes.Input,
			}

			found = true
			if err := storage.Write(
				tx,
				tokenKey(scope, task.Token),
				tokenRecord{Key: selected.Key, ActivityID: common.Value(task.Attributes.ActivityId)},
			); err != nil {
				return err
			}

			return storage.Write(tx, selected.Key, selected)
		})
		return found, err
	})
	return out, err
}

func decisionResult(
	tx storage.WriteTx,
	scope request.Scope,
	r *executionRecord,
	token *string,
	size *int64,
	reverse bool,
	previous, startAt int64,
) (*api.DecisionTask, error) {
	snapshot := *r
	snapshot.History = r.History[:r.DecisionStarted]
	if startAt > 0 {
		snapshot.History = snapshot.History[startAt-1:]
	}

	events, next, err := historyPage(new(snapshot), token, size, reverse)
	if err != nil {
		return nil, err
	}

	out := &api.DecisionTask{
		TaskToken:              new(r.DecisionToken),
		StartedEventId:         new(r.DecisionStarted),
		PreviousStartedEventId: new(previous),
		WorkflowExecution:      r.Detail.ExecutionInfo.Execution,
		WorkflowType:           r.Detail.ExecutionInfo.WorkflowType,
		Events:                 events,
	}

	if next != nil {
		pageID := r.DecisionToken + "/" + common.RandomID(32, "0123456789abcdef")
		if err := storage.Write(
			tx,
			scope.Key(serviceName, "page", pageID),
			decisionPage{
				Key:      r.Key,
				Token:    r.DecisionToken,
				After:    next,
				Previous: previous,
				Reverse:  reverse,
				Start:    startAt,
			},
		); err != nil {
			return nil, err
		}

		out.NextPageToken = new(pageID)
	}

	return out, nil
}

func (s *Service) PollForDecisionTask(
	ctx context.Context,
	in *api.PollForDecisionTaskInput,
) (*api.DecisionTask, error) {
	out := &api.DecisionTask{TaskToken: new("")}
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	err := s.longPoll(ctx, func() (bool, error) {
		found := false
		err := s.store.Update(ctx, func(tx storage.WriteTx) error {
			if _, err := readDomain(tx, scope, common.Value(in.Domain), false); err != nil {
				return err
			}

			if in.NextPageToken != nil {
				page, ok, err := storage.Read[decisionPage](tx, scope.Key(serviceName, "page", *in.NextPageToken))
				if err != nil {
					return err
				}

				if !ok {
					return invalid("Invalid nextPageToken")
				}

				r, _, err := readToken(tx, scope, new(page.Token), true)
				if err != nil {
					return err
				}

				if r.Domain != common.Value(in.Domain) ||
					common.Value(r.Detail.ExecutionConfiguration.TaskList.Name) != common.Value(in.TaskList.Name) {
					return invalid("Page belongs to a different task list")
				}

				out, err = decisionResult(
					tx,
					scope,
					r,
					page.After,
					in.MaximumPageSize,
					page.Reverse,
					page.Previous,
					page.Start,
				)
				found = true
				return err
			}

			rs, err := storage.Records[executionRecord](tx, executionPrefix(scope, common.Value(in.Domain)))
			if err != nil {
				return err
			}

			var selected *executionRecord
			for _, r := range rs {
				if !isOpen(r) || r.DecisionScheduled == 0 || r.DecisionStarted != 0 ||
					common.Value(r.Detail.ExecutionConfiguration.TaskList.Name) != common.Value(in.TaskList.Name) {
					continue
				}

				if selected == nil ||
					taskPriority(
						r.Detail.ExecutionConfiguration.TaskPriority,
					) > taskPriority(
						selected.Detail.ExecutionConfiguration.TaskPriority,
					) {
					selected = r
				}
			}

			if selected == nil {
				return nil
			}

			r := selected
			previous := r.PreviousStarted
			r.DecisionToken = common.RandomID(32, "0123456789abcdef")
			r.DecisionStarted = event(
				r,
				&api.DecisionTaskStartedEventAttributes{
					ScheduledEventId: new(r.DecisionScheduled),
					Identity:         in.Identity,
				},
			)
			r.DecisionStartedAt = now()
			r.PreviousStarted = r.DecisionStarted
			if writeErr := storage.Write(
				tx,
				tokenKey(scope, r.DecisionToken),
				tokenRecord{Key: r.Key},
			); writeErr != nil {
				return writeErr
			}

			startAt := int64(0)
			if common.Value(in.StartAtPreviousStartedEvent) {
				startAt = previous
			}

			out, err = decisionResult(
				tx,
				scope,
				r,
				nil,
				in.MaximumPageSize,
				common.Value(in.ReverseOrder),
				previous,
				startAt,
			)
			if err != nil {
				return err
			}

			found = true
			return storage.Write(tx, r.Key, r)
		})
		return found, err
	})
	return out, err
}

func clearDecision(tx storage.WriteTx, r *executionRecord) {
	tx.Delete(tokenKey(r.Scope, r.DecisionToken))
	for _, key := range tx.Keys(r.Scope.Prefix(serviceName, "page") + r.DecisionToken + "/") {
		tx.Delete(key)
	}

	r.DecisionToken = ""
	r.DecisionStarted = 0
	r.DecisionScheduled = 0
}

func finishActivity(tx storage.WriteTx, r *executionRecord, a *activity) {
	tx.Delete(tokenKey(r.Scope, a.Token))
	delete(r.Activities, common.Value(a.Attributes.ActivityId))
	scheduleDecision(r)
}

func (s *Service) RecordActivityTaskHeartbeat(
	ctx context.Context,
	in *api.RecordActivityTaskHeartbeatInput,
) (*api.ActivityTaskStatus, error) {
	out := &api.ActivityTaskStatus{}
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, a, err := readToken(tx, scope, in.TaskToken, false)
		if err != nil {
			return err
		}

		a.HeartbeatAt = now()
		a.Details = in.Details
		out.CancelRequested = new(a.CancelID != 0)
		return storage.Write(tx, r.Key, r)
	})
	return out, err
}

func (s *Service) RespondActivityTaskCompleted(
	ctx context.Context,
	in *api.RespondActivityTaskCompletedInput,
) (*api.RespondActivityTaskCompletedOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, a, err := readToken(tx, scope, in.TaskToken, false)
		if err != nil {
			return err
		}

		event(
			r,
			&api.ActivityTaskCompletedEventAttributes{
				ScheduledEventId: new(a.Scheduled),
				StartedEventId:   new(a.Started),
				Result:           in.Result,
			},
		)
		finishActivity(tx, r, a)
		return storage.Write(tx, r.Key, r)
	})
	return &api.RespondActivityTaskCompletedOutput{}, err
}

func (s *Service) RespondActivityTaskFailed(
	ctx context.Context,
	in *api.RespondActivityTaskFailedInput,
) (*api.RespondActivityTaskFailedOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, a, err := readToken(tx, scope, in.TaskToken, false)
		if err != nil {
			return err
		}

		event(
			r,
			&api.ActivityTaskFailedEventAttributes{
				ScheduledEventId: new(a.Scheduled),
				StartedEventId:   new(a.Started),
				Reason:           in.Reason,
				Details:          in.Details,
			},
		)
		finishActivity(tx, r, a)
		return storage.Write(tx, r.Key, r)
	})
	return &api.RespondActivityTaskFailedOutput{}, err
}

func (s *Service) RespondActivityTaskCanceled(
	ctx context.Context,
	in *api.RespondActivityTaskCanceledInput,
) (*api.RespondActivityTaskCanceledOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, a, err := readToken(tx, scope, in.TaskToken, false)
		if err != nil {
			return err
		}

		event(
			r,
			&api.ActivityTaskCanceledEventAttributes{
				ScheduledEventId:             new(a.Scheduled),
				StartedEventId:               new(a.Started),
				Details:                      in.Details,
				LatestCancelRequestedEventId: new(a.CancelID),
			},
		)
		finishActivity(tx, r, a)
		return storage.Write(tx, r.Key, r)
	})
	return &api.RespondActivityTaskCanceledOutput{}, err
}

func (s *Service) CountPendingActivityTasks(
	ctx context.Context,
	in *api.CountPendingActivityTasksInput,
) (*api.PendingTaskCount, error) {
	count := int64(0)
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := readDomain(tx, scope, common.Value(in.Domain), false); err != nil {
			return err
		}

		rs, err := storage.Records[executionRecord](tx, executionPrefix(scope, common.Value(in.Domain)))
		if err != nil {
			return err
		}

		for _, r := range rs {
			if !isOpen(r) {
				continue
			}

			for _, a := range r.Activities {
				if a.Started == 0 && common.Value(a.Attributes.TaskList.Name) == common.Value(in.TaskList.Name) {
					count++
				}
			}
		}

		return nil
	})
	return &api.PendingTaskCount{Count: new(count), Truncated: new(false)}, err
}

func (s *Service) CountPendingDecisionTasks(
	ctx context.Context,
	in *api.CountPendingDecisionTasksInput,
) (*api.PendingTaskCount, error) {
	count := int64(0)
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := readDomain(tx, scope, common.Value(in.Domain), false); err != nil {
			return err
		}

		rs, err := storage.Records[executionRecord](tx, executionPrefix(scope, common.Value(in.Domain)))
		if err != nil {
			return err
		}

		for _, r := range rs {
			if !isOpen(r) {
				continue
			}

			if r.DecisionScheduled != 0 && r.DecisionStarted == 0 &&
				common.Value(r.Detail.ExecutionConfiguration.TaskList.Name) == common.Value(in.TaskList.Name) {
				count++
			}
		}

		return nil
	})
	return &api.PendingTaskCount{Count: new(count), Truncated: new(false)}, err
}
