package glue

import (
	"context"
	"net/url"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/schedule"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindTrigger  = "trigger"
	triggerIndex = "aws/glue/index/trigger/"
)

type triggerRecord struct {
	Trigger  api.Trigger
	Tags     map[string]string
	Seen     map[string]bool
	Metadata request.Metadata
	Next     float64
}

type triggerReference struct {
	Scope request.Scope
	Name  string
}

func triggerIndexKey(scope request.Scope, n string) string {
	return triggerIndex + scope.AccountID + "/" + scope.Region + "/" + url.PathEscape(n)
}

func loadTrigger(tx storage.ReadTx, scope request.Scope, n string) (*triggerRecord, error) {
	if err := orchestrationName(new(n)); err != nil {
		return nil, err
	}

	r, ok, err := storage.Read[triggerRecord](tx, orchestrationKey(scope, kindTrigger, n))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, notFound("Trigger %s not found", n)
	}

	return r, nil
}

func validateTrigger(tx storage.ReadTx, scope request.Scope, t *api.Trigger) error {
	if err := orchestrationName(t.Name); err != nil {
		return err
	}

	if t.Description != nil && len(*t.Description) > 2048 {
		return invalid("Description exceeds 2048 bytes")
	}

	typ := common.Value(t.Type)
	if typ != triggerOnDemand && typ != triggerScheduled && typ != triggerConditional {
		return invalid("Unsupported trigger Type")
	}

	if len(t.Actions) == 0 {
		return invalid("Actions must not be empty")
	}

	for _, a := range t.Actions {
		if _, err := loadJob(tx, scope, common.Value(a.JobName)); err != nil {
			return err
		}

		if err := validateArguments(a.Arguments); err != nil {
			return err
		}

		if err := validateTimeout(a.Timeout); err != nil {
			return err
		}
	}

	if typ == triggerScheduled {
		if !strings.HasPrefix(common.Value(t.Schedule), "cron(") {
			return invalid("Scheduled triggers require cron(...)")
		}

		plan, err := schedule.Parse(*t.Schedule)
		if err != nil {
			return invalid("Invalid Glue cron: %s", err)
		}

		// Check a complete day's worth of occurrences, including hour boundaries.
		// The shared parser skips directly between eligible days/months/years.
		previous, ok := plan.First(time.Date(1970, time.January, 1, 0, 0, 0, 0, time.UTC))
		for range 1440 {
			if !ok {
				break
			}

			next, exists := plan.First(previous)
			if exists && next.Sub(previous) < 5*time.Minute {
				return invalid("Glue schedules must be at least five minutes apart")
			}

			previous, ok = next, exists
		}
	} else if t.Schedule != nil {
		return invalid("Schedule requires a SCHEDULED trigger")
	}

	if typ == triggerConditional {
		if t.Predicate == nil || len(t.Predicate.Conditions) == 0 {
			return invalid("Conditional triggers require a Predicate")
		}

		logical := common.Value(t.Predicate.Logical)
		if logical != "" && logical != "AND" && logical != "ANY" {
			return invalid("Predicate.Logical must be AND or ANY")
		}

		for _, c := range t.Predicate.Conditions {
			if c.LogicalOperator != nil && *c.LogicalOperator != "EQUALS" {
				return invalid("LogicalOperator must be EQUALS")
			}

			state := common.Value(c.State)
			if state != stateSucceeded && state != stateFailed && state != stateStopped && state != stateTimeout {
				return invalid("Unsupported conditional job state")
			}

			if _, err := loadJob(tx, scope, common.Value(c.JobName)); err != nil {
				return err
			}
		}
	} else if t.Predicate != nil {
		return invalid("Predicate requires a CONDITIONAL trigger")
	}

	if t.WorkflowName != nil {
		if _, err := loadWorkflow(tx, scope, *t.WorkflowName); err != nil {
			return err
		}
	}

	return nil
}

func (s *Service) CreateTrigger(ctx context.Context, in *api.CreateTriggerRequest) (*api.CreateTriggerResponse, error) {
	if err := checkTags(in.Tags); err != nil {
		return nil, err
	}

	if in.StartOnCreation != nil && *in.StartOnCreation && common.Value(in.Type) == triggerOnDemand {
		return nil, invalid("ON_DEMAND triggers cannot StartOnCreation")
	}

	metadata, _ := request.FromContext(ctx)
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		t := &api.Trigger{
			Name:         in.Name,
			WorkflowName: in.WorkflowName,
			Type:         in.Type,
			Schedule:     in.Schedule,
			Predicate:    in.Predicate,
			Actions:      in.Actions,
			Description:  in.Description,
			State:        new("CREATED"),
			Id:           new(common.RandomID(32)),
		}

		if err := validateTrigger(tx, scope, t); err != nil {
			return err
		}

		key := orchestrationKey(scope, kindTrigger, *in.Name)
		if _, ok := tx.Get(key); ok {
			return alreadyExists("Trigger already exists")
		}

		r := &triggerRecord{Trigger: *t, Tags: in.Tags, Metadata: metadata, Seen: map[string]bool{}}
		if common.Value(in.StartOnCreation) {
			s.activateTrigger(r)
		}

		if err := storage.Write(tx, key, r); err != nil {
			return err
		}

		return storage.Write(tx, triggerIndexKey(scope, *in.Name), triggerReference{Scope: scope, Name: *in.Name})
	})
	return &api.CreateTriggerResponse{Name: in.Name}, err
}

func (s *Service) activateTrigger(r *triggerRecord) {
	r.Trigger.State = new(triggerActivated)
	if common.Value(r.Trigger.Type) == triggerScheduled {
		plan, err := schedule.Parse(common.Value(r.Trigger.Schedule))
		if err != nil {
			return
		}

		next, ok := plan.First(s.now())
		if ok {
			r.Next = common.Unix(next)
		}
	}
}

func (s *Service) GetTrigger(ctx context.Context, in *api.GetTriggerRequest) (*api.GetTriggerResponse, error) {
	out := &api.GetTriggerResponse{}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		r, err := loadTrigger(tx, scope, common.Value(in.Name))
		if err != nil {
			return err
		}

		out.Trigger = &r.Trigger
		return nil
	})
	return out, err
}

func (s *Service) UpdateTrigger(ctx context.Context, in *api.UpdateTriggerRequest) (*api.UpdateTriggerResponse, error) {
	if in.TriggerUpdate == nil {
		return nil, invalid("TriggerUpdate is required")
	}

	out := &api.UpdateTriggerResponse{}
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := loadTrigger(tx, scope, common.Value(in.Name))
		if err != nil {
			return err
		}

		u := in.TriggerUpdate
		if u.Name != nil && *u.Name != *in.Name {
			return invalid("Renaming triggers is not supported")
		}

		r.Trigger.Description, r.Trigger.Schedule = u.Description, u.Schedule
		r.Trigger.Actions, r.Trigger.Predicate = u.Actions, u.Predicate
		if err := validateTrigger(tx, scope, &r.Trigger); err != nil {
			return err
		}

		if common.Value(r.Trigger.State) == triggerActivated {
			s.activateTrigger(r)
		}

		out.Trigger = &r.Trigger
		return storage.Write(tx, orchestrationKey(scope, kindTrigger, *in.Name), r)
	})
	return out, err
}

func (s *Service) DeleteTrigger(ctx context.Context, in *api.DeleteTriggerRequest) (*api.DeleteTriggerResponse, error) {
	if err := orchestrationName(in.Name); err != nil {
		return nil, err
	}

	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		tx.Delete(orchestrationKey(scope, kindTrigger, *in.Name))
		tx.Delete(triggerIndexKey(scope, *in.Name))
		return nil
	})
	return &api.DeleteTriggerResponse{Name: in.Name}, err
}

func (s *Service) StartTrigger(ctx context.Context, in *api.StartTriggerRequest) (*api.StartTriggerResponse, error) {
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := loadTrigger(tx, scope, common.Value(in.Name))
		if err != nil {
			return err
		}

		if common.Value(r.Trigger.Type) == triggerOnDemand {
			if err := s.startTriggerActions(tx, scope, r); err != nil {
				return err
			}
		} else {
			s.activateTrigger(r)
		}

		return storage.Write(tx, orchestrationKey(scope, kindTrigger, *in.Name), r)
	})
	return &api.StartTriggerResponse{Name: in.Name}, err
}

func (s *Service) StopTrigger(ctx context.Context, in *api.StopTriggerRequest) (*api.StopTriggerResponse, error) {
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := loadTrigger(tx, scope, common.Value(in.Name))
		if err != nil {
			return err
		}

		if common.Value(r.Trigger.Type) == triggerOnDemand {
			return invalid("ON_DEMAND triggers cannot be stopped")
		}

		r.Trigger.State = new("DEACTIVATED")
		return storage.Write(tx, orchestrationKey(scope, kindTrigger, *in.Name), r)
	})
	return &api.StopTriggerResponse{Name: in.Name}, err
}

func (s *Service) triggerPage(
	ctx context.Context,
	tags map[string]string,
	dependent, token *string,
	limit *int64,
) ([]*triggerRecord, *string, error) {
	var all []*triggerRecord
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		rows, err := storage.Records[triggerRecord](tx, scope.Prefix(service, kindTrigger))
		if err != nil {
			return err
		}

		for _, r := range rows {
			match := true
			for k, v := range tags {
				if value, ok := r.Tags[k]; !ok || value != v {
					match = false
				}
			}

			if dependent != nil {
				found := false
				if r.Trigger.Predicate != nil {
					for _, c := range r.Trigger.Predicate.Conditions {
						if common.Value(c.JobName) == *dependent {
							found = true
						}
					}
				}

				match = match && found
			}

			if match {
				all = append(all, r)
			}
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return orchestrationPage(all, func(r *triggerRecord) string { return *r.Trigger.Name }, token, limit)
}

func (s *Service) GetTriggers(ctx context.Context, in *api.GetTriggersRequest) (*api.GetTriggersResponse, error) {
	rows, next, err := s.triggerPage(ctx, nil, in.DependentJobName, in.NextToken, in.MaxResults)
	out := &api.GetTriggersResponse{Triggers: []api.Trigger{}, NextToken: next}
	for _, r := range rows {
		out.Triggers = append(out.Triggers, r.Trigger)
	}

	return out, err
}

func (s *Service) ListTriggers(ctx context.Context, in *api.ListTriggersRequest) (*api.ListTriggersResponse, error) {
	rows, next, err := s.triggerPage(ctx, in.Tags, in.DependentJobName, in.NextToken, in.MaxResults)
	out := &api.ListTriggersResponse{TriggerNames: []string{}, NextToken: next}
	for _, r := range rows {
		out.TriggerNames = append(out.TriggerNames, *r.Trigger.Name)
	}

	return out, err
}

func (s *Service) BatchGetTriggers(
	ctx context.Context,
	in *api.BatchGetTriggersRequest,
) (*api.BatchGetTriggersResponse, error) {
	if len(in.TriggerNames) < 1 {
		return nil, invalid("TriggerNames must not be empty")
	}

	out := &api.BatchGetTriggersResponse{Triggers: []api.Trigger{}, TriggersNotFound: []string{}}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		for _, n := range in.TriggerNames {
			r, ok, err := storage.Read[triggerRecord](tx, orchestrationKey(scope, kindTrigger, n))
			if err != nil {
				return err
			}

			if ok {
				out.Triggers = append(out.Triggers, r.Trigger)
			} else {
				out.TriggersNotFound = append(out.TriggersNotFound, n)
			}
		}

		return nil
	})
	return out, err
}

func (s *Service) fireTrigger(tx storage.WriteTx, scope request.Scope, r *triggerRecord, workflow, run string) error {
	for _, a := range r.Trigger.Actions {
		_, err := s.startRun(
			tx,
			scope,
			&api.StartJobRunRequest{JobName: a.JobName, Arguments: a.Arguments, Timeout: a.Timeout},
			&r.Metadata,
			*r.Trigger.Name,
			workflow,
			run,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Service) tickTriggers(ctx context.Context) {
	var refs []*triggerReference
	_ = s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		refs, err = storage.Records[triggerReference](tx, triggerIndex)
		return err
	})
	for _, ref := range refs {
		_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
			r, err := loadTrigger(tx, ref.Scope, ref.Name)
			if err != nil {
				return err
			}

			if common.Value(r.Trigger.State) != triggerActivated {
				return nil
			}

			typ := common.Value(r.Trigger.Type)
			if r.Trigger.WorkflowName != nil && typ == triggerConditional {
				return nil
			}

			if typ == triggerScheduled && r.Next > 0 && common.Unix(s.now()) >= r.Next {
				if err := s.startTriggerActions(tx, ref.Scope, r); err != nil {
					return err
				}

				plan, err := schedule.Parse(*r.Trigger.Schedule)
				if err != nil {
					return err
				}

				next, ok := plan.After(time.UnixMilli(int64(r.Next*1000)), s.now())
				r.Next = 0
				if ok {
					r.Next = common.Unix(next)
				}
			} else if typ == triggerConditional {
				signature, match, err := predicateMatch(tx, ref.Scope, r, "", "")
				if err != nil {
					return err
				}

				if match && !r.Seen[signature] {
					if err := s.fireTrigger(tx, ref.Scope, r, "", ""); err != nil {
						return err
					}

					if r.Seen == nil {
						r.Seen = map[string]bool{}
					}

					r.Seen[signature] = true
				}
			}

			return storage.Write(tx, orchestrationKey(ref.Scope, kindTrigger, ref.Name), r)
		})
	}

	s.tickWorkflows(ctx)
}

func predicateMatch(
	tx storage.ReadTx,
	scope request.Scope,
	t *triggerRecord,
	workflow, run string,
) (signature string, matched bool, err error) {
	if t.Trigger.Predicate == nil {
		return "", false, nil
	}

	signature = ""
	matches := 0
	for _, c := range t.Trigger.Predicate.Conditions {
		rows, err := storage.Records[runRecord](
			tx,
			scope.Key(service, "run", url.PathEscape(common.Value(c.JobName))+"/"),
		)
		if err != nil {
			return "", false, err
		}

		var latest *runRecord
		for _, r := range rows {
			if r.Workflow != workflow || r.WorkflowRun != run || r.Run.TriggerName == nil {
				continue
			}

			if latest == nil || common.Value(r.Run.StartedOn) > common.Value(latest.Run.StartedOn) {
				latest = r
			}
		}

		if latest != nil && common.Value(latest.Run.JobRunState) == common.Value(c.State) {
			matches++
			signature += *latest.Run.Id + "/"
		}
	}

	required := len(t.Trigger.Predicate.Conditions)
	if common.Value(t.Trigger.Predicate.Logical) == "ANY" {
		required = 1
	}

	return signature, matches >= required, nil
}

// Starting a workflow's source trigger creates a run; downstream actions retain that run's identity.
func (s *Service) startTriggerActions(tx storage.WriteTx, scope request.Scope, r *triggerRecord) error {
	if r.Trigger.WorkflowName != nil {
		_, err := s.startWorkflow(tx, scope, &api.StartWorkflowRunRequest{Name: r.Trigger.WorkflowName}, r)
		return err
	}

	return s.fireTrigger(tx, scope, r, "", "")
}
