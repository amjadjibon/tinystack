package glue

import (
	"context"
	"maps"
	"net/url"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindWorkflow  = "workflow"
	workflowIndex = "aws/glue/index/workflow/"
)

type workflowRecord struct {
	Workflow api.Workflow
	Tags     map[string]string
}

type workflowRunRecord struct {
	Run   api.WorkflowRun
	Fired map[string]bool
}

type workflowReference struct {
	Scope request.Scope
	Name  string
	ID    string
}

func loadWorkflow(tx storage.ReadTx, scope request.Scope, n string) (*workflowRecord, error) {
	if err := orchestrationName(new(n)); err != nil {
		return nil, err
	}

	r, ok, err := storage.Read[workflowRecord](tx, orchestrationKey(scope, kindWorkflow, n))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, notFound("Workflow %s not found", n)
	}

	return r, nil
}

func workflowRunKey(scope request.Scope, n, id string) string {
	return scope.Key(service, "workflow-run", url.PathEscape(n)+"/"+id)
}

func workflowIndexKey(ref workflowReference) string {
	return workflowIndex + ref.Scope.AccountID + "/" + ref.Scope.Region + "/" + url.PathEscape(ref.Name) + "/" + ref.ID
}

func loadWorkflowRun(tx storage.ReadTx, scope request.Scope, n, id string) (*workflowRunRecord, error) {
	if err := orchestrationName(new(n)); err != nil {
		return nil, err
	}

	if err := orchestrationName(new(id)); err != nil {
		return nil, err
	}

	r, ok, err := storage.Read[workflowRunRecord](tx, workflowRunKey(scope, n, id))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, notFound("Workflow run not found")
	}

	return r, nil
}

func (s *Service) CreateWorkflow(
	ctx context.Context,
	in *api.CreateWorkflowRequest,
) (*api.CreateWorkflowResponse, error) {
	if err := orchestrationName(in.Name); err != nil {
		return nil, err
	}

	if err := workflowProperties(in.Description, in.DefaultRunProperties); err != nil {
		return nil, err
	}

	if err := checkTags(in.Tags); err != nil {
		return nil, err
	}

	if in.MaxConcurrentRuns != nil && *in.MaxConcurrentRuns < 1 {
		return nil, invalid("MaxConcurrentRuns must be at least 1")
	}

	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		key := orchestrationKey(scope, kindWorkflow, *in.Name)
		if _, ok := tx.Get(key); ok {
			return alreadyExists("Workflow already exists")
		}

		now := common.Unix(s.now())
		return storage.Write(tx, key, &workflowRecord{Tags: in.Tags, Workflow: api.Workflow{
			Name:                 in.Name,
			Description:          in.Description,
			DefaultRunProperties: in.DefaultRunProperties,
			MaxConcurrentRuns:    in.MaxConcurrentRuns,
			CreatedOn:            new(now),
			LastModifiedOn:       new(now),
		}})
	})
	return &api.CreateWorkflowResponse{Name: in.Name}, err
}

func (s *Service) UpdateWorkflow(
	ctx context.Context,
	in *api.UpdateWorkflowRequest,
) (*api.UpdateWorkflowResponse, error) {
	if err := workflowProperties(in.Description, in.DefaultRunProperties); err != nil {
		return nil, err
	}

	if in.MaxConcurrentRuns != nil && *in.MaxConcurrentRuns < 1 {
		return nil, invalid("MaxConcurrentRuns must be at least 1")
	}

	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := loadWorkflow(tx, scope, common.Value(in.Name))
		if err != nil {
			return err
		}

		if in.Description != nil {
			r.Workflow.Description = in.Description
		}

		if in.DefaultRunProperties != nil {
			r.Workflow.DefaultRunProperties = in.DefaultRunProperties
		}

		if in.MaxConcurrentRuns != nil {
			r.Workflow.MaxConcurrentRuns = in.MaxConcurrentRuns
		}

		r.Workflow.LastModifiedOn = new(common.Unix(s.now()))
		return storage.Write(tx, orchestrationKey(scope, kindWorkflow, *in.Name), r)
	})
	return &api.UpdateWorkflowResponse{Name: in.Name}, err
}

func (s *Service) DeleteWorkflow(
	ctx context.Context,
	in *api.DeleteWorkflowRequest,
) (*api.DeleteWorkflowResponse, error) {
	if err := orchestrationName(in.Name); err != nil {
		return nil, err
	}

	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		tx.Delete(orchestrationKey(scope, kindWorkflow, *in.Name))
		return nil
	})
	return &api.DeleteWorkflowResponse{Name: in.Name}, err
}

func workflowGraph(tx storage.ReadTx, scope request.Scope, n, run string) (*api.WorkflowGraph, error) {
	triggers, err := storage.Records[triggerRecord](tx, scope.Prefix(service, kindTrigger))
	if err != nil {
		return nil, err
	}

	graph := &api.WorkflowGraph{Nodes: []api.Node{}, Edges: []api.Edge{}}
	jobs := map[string]bool{}
	for _, r := range triggers {
		t := &r.Trigger
		if common.Value(t.WorkflowName) != n {
			continue
		}

		tid := "trigger/" + *t.Name
		graph.Nodes = append(graph.Nodes, api.Node{
			Type: new("TRIGGER"), Name: t.Name, UniqueId: new(tid),
			TriggerDetails: &api.TriggerNodeDetails{Trigger: t},
		})
		for _, a := range t.Actions {
			job := common.Value(a.JobName)
			graph.Edges = append(graph.Edges, api.Edge{SourceId: new(tid), DestinationId: new("job/" + job)})
			jobs[job] = true
		}

		if t.Predicate != nil {
			for _, c := range t.Predicate.Conditions {
				job := common.Value(c.JobName)
				graph.Edges = append(graph.Edges, api.Edge{SourceId: new("job/" + job), DestinationId: new(tid)})
				jobs[job] = true
			}
		}
	}

	// Use job records, rather than a map iteration, to retain storage key order.
	records, err := storage.Records[jobRecord](tx, scope.Prefix(service, kindJob))
	if err != nil {
		return nil, err
	}

	for _, j := range records {
		name := *j.Job.Name
		if !jobs[name] {
			continue
		}

		details := &api.JobNodeDetails{}
		if run != "" {
			runs, err := storage.Records[runRecord](tx, scope.Key(service, "run", url.PathEscape(name)+"/"))
			if err != nil {
				return nil, err
			}

			for _, r := range runs {
				if r.Workflow == n && r.WorkflowRun == run {
					details.JobRuns = append(details.JobRuns, r.Run)
				}
			}
		}

		graph.Nodes = append(
			graph.Nodes,
			api.Node{Type: new("JOB"), Name: new(name), UniqueId: new("job/" + name), JobDetails: details},
		)
	}

	return graph, nil
}

func (s *Service) GetWorkflow(ctx context.Context, in *api.GetWorkflowRequest) (*api.GetWorkflowResponse, error) {
	out := &api.GetWorkflowResponse{}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		r, err := loadWorkflow(tx, scope, common.Value(in.Name))
		if err != nil {
			return err
		}

		if common.Value(in.IncludeGraph) {
			r.Workflow.Graph, err = workflowGraph(tx, scope, *in.Name, "")
			if err != nil {
				return err
			}
		}

		out.Workflow = &r.Workflow
		return nil
	})
	return out, err
}

func (s *Service) ListWorkflows(ctx context.Context, in *api.ListWorkflowsRequest) (*api.ListWorkflowsResponse, error) {
	if in.MaxResults != nil && (*in.MaxResults < 1 || *in.MaxResults > 25) {
		return nil, invalid("MaxResults must be between 1 and 25")
	}

	var rows []*workflowRecord
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		rows, err = storage.Records[workflowRecord](tx, scope.Prefix(service, kindWorkflow))
		return err
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := orchestrationPage(
		rows,
		func(r *workflowRecord) string { return *r.Workflow.Name },
		in.NextToken,
		in.MaxResults,
	)
	out := &api.ListWorkflowsResponse{Workflows: []string{}, NextToken: next}
	for _, r := range selected {
		out.Workflows = append(out.Workflows, *r.Workflow.Name)
	}

	return out, err
}

func (s *Service) BatchGetWorkflows(
	ctx context.Context,
	in *api.BatchGetWorkflowsRequest,
) (*api.BatchGetWorkflowsResponse, error) {
	if len(in.Names) < 1 || len(in.Names) > 25 {
		return nil, invalid("Names must contain 1 to 25 names")
	}

	out := &api.BatchGetWorkflowsResponse{Workflows: []api.Workflow{}, MissingWorkflows: []string{}}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		for _, n := range in.Names {
			r, ok, err := storage.Read[workflowRecord](tx, orchestrationKey(scope, kindWorkflow, n))
			if err != nil {
				return err
			}

			if !ok {
				out.MissingWorkflows = append(out.MissingWorkflows, n)
				continue
			}

			if common.Value(in.IncludeGraph) {
				r.Workflow.Graph, err = workflowGraph(tx, scope, n, "")
				if err != nil {
					return err
				}
			}

			out.Workflows = append(out.Workflows, r.Workflow)
		}

		return nil
	})
	return out, err
}

func (s *Service) StartWorkflowRun(
	ctx context.Context,
	in *api.StartWorkflowRunRequest,
) (*api.StartWorkflowRunResponse, error) {
	if err := workflowProperties(nil, in.RunProperties); err != nil {
		return nil, err
	}

	out := &api.StartWorkflowRunResponse{}
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		id, err := s.startWorkflow(tx, scope, in, nil)
		if err != nil {
			return err
		}

		out.RunId = new(id)
		return nil
	})
	return out, err
}

func (s *Service) startWorkflow(tx storage.WriteTx, scope request.Scope, in *api.StartWorkflowRunRequest,
	start *triggerRecord,
) (string, error) {
	w, err := loadWorkflow(tx, scope, common.Value(in.Name))
	if err != nil {
		return "", err
	}

	runs, err := storage.Records[workflowRunRecord](
		tx,
		scope.Key(service, "workflow-run", url.PathEscape(*in.Name)+"/"),
	)
	if err != nil {
		return "", err
	}

	active := int64(0)
	for _, r := range runs {
		if common.Value(r.Run.Status) == stateRunning || common.Value(r.Run.Status) == stateStopping {
			active++
		}
	}

	if w.Workflow.MaxConcurrentRuns != nil && active >= *w.Workflow.MaxConcurrentRuns {
		return "", awserror.New("ConcurrentRunsExceededException", "Workflow concurrent runs exceeded")
	}

	if start == nil {
		triggers, readErr := storage.Records[triggerRecord](tx, scope.Prefix(service, kindTrigger))
		if readErr != nil {
			return "", readErr
		}

		for _, t := range triggers {
			if common.Value(t.Trigger.WorkflowName) == *in.Name && common.Value(t.Trigger.Type) == triggerOnDemand {
				if start != nil {
					return "", invalid("Workflow must have exactly one ON_DEMAND start trigger")
				}

				start = t
			}
		}
	}

	if start == nil {
		return "", invalid("Workflow has no ON_DEMAND start trigger")
	}

	id := "wr_" + common.RandomID(64, "0123456789abcdef")
	props := common.CloneTags(w.Workflow.DefaultRunProperties)
	if props == nil {
		props = map[string]string{}
	}

	maps.Copy(props, in.RunProperties)
	r := &workflowRunRecord{Run: api.WorkflowRun{
		Name: in.Name, WorkflowRunId: new(id), WorkflowRunProperties: props,
		StartedOn: new(common.Unix(s.now())), Status: new(stateRunning),
	}, Fired: map[string]bool{*start.Trigger.Name: true}}
	if err := s.fireTrigger(tx, scope, start, *in.Name, id); err != nil {
		return "", err
	}

	if err := storage.Write(tx, workflowRunKey(scope, *in.Name, id), r); err != nil {
		return "", err
	}

	ref := workflowReference{Scope: scope, Name: *in.Name, ID: id}
	if err := storage.Write(tx, workflowIndexKey(ref), ref); err != nil {
		return "", err
	}

	w.Workflow.LastRun = &r.Run
	if err := storage.Write(tx, orchestrationKey(scope, kindWorkflow, *in.Name), w); err != nil {
		return "", err
	}

	return id, nil
}

func (s *Service) GetWorkflowRun(
	ctx context.Context,
	in *api.GetWorkflowRunRequest,
) (*api.GetWorkflowRunResponse, error) {
	out := &api.GetWorkflowRunResponse{}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		r, err := loadWorkflowRun(tx, scope, common.Value(in.Name), common.Value(in.RunId))
		if err != nil {
			return err
		}

		if common.Value(in.IncludeGraph) {
			r.Run.Graph, err = workflowGraph(tx, scope, *in.Name, *in.RunId)
			if err != nil {
				return err
			}
		}

		out.Run = &r.Run
		return nil
	})
	return out, err
}

func (s *Service) GetWorkflowRuns(
	ctx context.Context,
	in *api.GetWorkflowRunsRequest,
) (*api.GetWorkflowRunsResponse, error) {
	var rows []*workflowRunRecord
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		if _, err := loadWorkflow(tx, scope, common.Value(in.Name)); err != nil {
			return err
		}

		var err error
		rows, err = storage.Records[workflowRunRecord](
			tx,
			scope.Key(service, "workflow-run", url.PathEscape(*in.Name)+"/"),
		)
		if err != nil {
			return err
		}

		if common.Value(in.IncludeGraph) {
			for _, r := range rows {
				r.Run.Graph, err = workflowGraph(tx, scope, *in.Name, *r.Run.WorkflowRunId)
				if err != nil {
					return err
				}
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(
		rows,
		func(r *workflowRunRecord) string { return *r.Run.WorkflowRunId },
		in.NextToken,
		in.MaxResults,
	)
	out := &api.GetWorkflowRunsResponse{Runs: []api.WorkflowRun{}, NextToken: next}
	for _, r := range selected {
		out.Runs = append(out.Runs, r.Run)
	}

	return out, err
}

func (s *Service) GetWorkflowRunProperties(
	ctx context.Context,
	in *api.GetWorkflowRunPropertiesRequest,
) (*api.GetWorkflowRunPropertiesResponse, error) {
	run, err := s.GetWorkflowRun(ctx, &api.GetWorkflowRunRequest{Name: in.Name, RunId: in.RunId})
	if err != nil {
		return nil, err
	}

	return &api.GetWorkflowRunPropertiesResponse{RunProperties: run.Run.WorkflowRunProperties}, nil
}

func (s *Service) PutWorkflowRunProperties(
	ctx context.Context,
	in *api.PutWorkflowRunPropertiesRequest,
) (*api.PutWorkflowRunPropertiesResponse, error) {
	if err := workflowProperties(nil, in.RunProperties); err != nil {
		return nil, err
	}

	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := loadWorkflowRun(tx, scope, common.Value(in.Name), common.Value(in.RunId))
		if err != nil {
			return err
		}

		if r.Run.WorkflowRunProperties == nil {
			r.Run.WorkflowRunProperties = map[string]string{}
		}

		maps.Copy(r.Run.WorkflowRunProperties, in.RunProperties)
		return storage.Write(tx, workflowRunKey(scope, *in.Name, *in.RunId), r)
	})
	return &api.PutWorkflowRunPropertiesResponse{}, err
}

func (s *Service) StopWorkflowRun(
	ctx context.Context,
	in *api.StopWorkflowRunRequest,
) (*api.StopWorkflowRunResponse, error) {
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := loadWorkflowRun(tx, scope, common.Value(in.Name), common.Value(in.RunId))
		if err != nil {
			return err
		}

		if common.Value(r.Run.Status) != stateRunning {
			return invalid("Workflow run is not RUNNING")
		}

		r.Run.Status = new(stateStopping)
		return storage.Write(tx, workflowRunKey(scope, *in.Name, *in.RunId), r)
	})
	return &api.StopWorkflowRunResponse{}, err
}

func (s *Service) tickWorkflows(ctx context.Context) {
	var refs []*workflowReference
	_ = s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		refs, err = storage.Records[workflowReference](tx, workflowIndex)
		return err
	})
	for _, ref := range refs {
		_ = s.store.Update(ctx, func(tx storage.WriteTx) error { return s.advanceWorkflow(tx, *ref) })
	}
}

func (s *Service) advanceWorkflow(tx storage.WriteTx, ref workflowReference) error {
	r, err := loadWorkflowRun(tx, ref.Scope, ref.Name, ref.ID)
	if err != nil {
		return err
	}

	stopping := common.Value(r.Run.Status) == stateStopping
	if !stopping {
		triggers, readErr := storage.Records[triggerRecord](tx, ref.Scope.Prefix(service, kindTrigger))
		if readErr != nil {
			return readErr
		}

		for _, t := range triggers {
			if common.Value(t.Trigger.WorkflowName) != ref.Name || common.Value(t.Trigger.Type) != triggerConditional ||
				common.Value(t.Trigger.State) != triggerActivated || r.Fired[*t.Trigger.Name] {
				continue
			}

			_, match, matchErr := predicateMatch(tx, ref.Scope, t, ref.Name, ref.ID)
			if matchErr != nil {
				return matchErr
			}

			if match {
				if fireErr := s.fireTrigger(tx, ref.Scope, t, ref.Name, ref.ID); fireErr != nil {
					return fireErr
				}

				r.Fired[*t.Trigger.Name] = true
			}
		}
	}

	runs, err := storage.Records[runRecord](tx, ref.Scope.Prefix(service, "run"))
	if err != nil {
		return err
	}

	counts := map[string]int64{}
	total := int64(0)
	running := int64(0)
	for _, run := range runs {
		if run.Workflow != ref.Name || run.WorkflowRun != ref.ID {
			continue
		}

		state := common.Value(run.Run.JobRunState)
		counts[state]++
		total++
		if activeRun(state) {
			running++
			if stopping {
				run.Run.JobRunState = new(stateStopping)
				if writeErr := storage.Write(
					tx,
					runKey(ref.Scope, *run.Run.JobName, *run.Run.Id),
					run,
				); writeErr != nil {
					return writeErr
				}
			}
		}
	}

	r.Run.Statistics = &api.WorkflowRunStatistics{
		TotalActions:   new(total),
		RunningActions: new(running),
		SucceededActions: new(
			counts[stateSucceeded],
		),
		FailedActions:  new(counts[stateFailed]),
		TimeoutActions: new(counts[stateTimeout]),
		StoppedActions: new(counts[stateStopped]),
	}

	if running == 0 {
		status := "COMPLETED"
		if stopping {
			status = stateStopped
		}

		r.Run.Status, r.Run.CompletedOn = new(status), new(common.Unix(s.now()))
		tx.Delete(workflowIndexKey(ref))
	}

	w, err := loadWorkflow(tx, ref.Scope, ref.Name)
	if err == nil {
		w.Workflow.LastRun = &r.Run
		if err := storage.Write(tx, orchestrationKey(ref.Scope, kindWorkflow, ref.Name), w); err != nil {
			return err
		}
	}

	return storage.Write(tx, workflowRunKey(ref.Scope, ref.Name, ref.ID), r)
}

func workflowProperties(description *string, properties map[string]string) error {
	if description != nil && len(*description) > 120000 {
		return invalid("Workflow Description exceeds 120000 bytes")
	}

	for k := range properties {
		if err := orchestrationName(new(k)); err != nil {
			return err
		}
	}

	return nil
}
