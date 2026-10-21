package emr

import (
	"context"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/emr"
	"github.com/amjadjibon/tinystack/internal/common"
)

func checkStates(states, allowed []string) error {
	for _, st := range states {
		if !slices.Contains(allowed, st) {
			return invalid("Invalid state %s.", st)
		}
	}

	return nil
}

func finishStep(st *api.Step, status string) {
	st.Status.State = new(status)
	st.Status.Timeline.EndDateTime = common.SecondsMilli(time.Now())
	st.Status.StateChangeReason = &api.StepStateChangeReason{Message: new(simulated)}
}

func addSteps(r *record, steps []api.StepConfig) ([]string, error) {
	ids := []string{}
	for _, v := range steps {
		action := common.OrElse(v.ActionOnFailure, new(continueSteps))
		if !slices.Contains([]string{continueSteps, cancelAndWait, terminateCluster}, *action) {
			return nil, invalid("Invalid ActionOnFailure.")
		}

		if *r.Cluster.StepConcurrencyLevel > 1 && *action != continueSteps {
			return nil, invalid("ActionOnFailure must be CONTINUE when StepConcurrencyLevel exceeds 1.")
		}

		if strings.TrimSpace(common.Value(v.Name)) == "" || v.HadoopJarStep == nil ||
			common.Value(v.HadoopJarStep.Jar) == "" {
			return nil, invalid("Steps require Name and HadoopJarStep.Jar.")
		}

		props := map[string]string{}
		for _, p := range v.HadoopJarStep.Properties {
			props[common.Value(p.Key)] = common.Value(p.Value)
		}

		id := "s-" + strings.ToUpper(common.RandomID(13))
		r.Steps = append(r.Steps, api.Step{
			Id: new(id), Name: v.Name, ActionOnFailure: action,
			Config: &api.HadoopStepConfig{
				Jar: v.HadoopJarStep.Jar, MainClass: v.HadoopJarStep.MainClass,
				Args: v.HadoopJarStep.Args, Properties: props,
			}, Status: &api.StepStatus{
				State:             new(pending),
				StateChangeReason: &api.StepStateChangeReason{Message: new(simulated)},
				Timeline:          &api.StepTimeline{CreationDateTime: common.SecondsMilli(time.Now())},
			},
		})
		ids = append(ids, id)
	}

	r.IdleSince = 0
	r.PauseAfterFailure = false
	return ids, nil
}

// applyFailure implements failure policy independently of execution. Simulated steps normally complete;
// a persisted FAILED step applies its policy on the next read, without executing user code.
func applyFailure(r *record, st *api.Step) {
	if *r.Cluster.StepConcurrencyLevel > 1 {
		return
	}

	switch common.Value(st.ActionOnFailure) {
	case cancelAndWait:
		r.PauseAfterFailure = true
		for i := range r.Steps {
			if *r.Steps[i].Status.State == pending {
				finishStep(&r.Steps[i], cancelled)
			}
		}

		state(r, waiting, "STEP_FAILURE")
	case terminateCluster:
		state(r, terminating, "STEP_FAILURE")
	}
}

func (s *Service) AddJobFlowSteps(
	ctx context.Context,
	in *api.AddJobFlowStepsInput,
) (*api.AddJobFlowStepsOutput, error) {
	if len(in.Steps) == 0 {
		return nil, invalid("Steps is required.")
	}

	out := &api.AddJobFlowStepsOutput{}
	_, err := s.change(ctx, in.JobFlowId, func(r *record) error {
		if e := mutable(r); e != nil {
			return e
		}

		var e error
		out.StepIds, e = addSteps(r, in.Steps)
		return e
	})
	return out, err
}

func (s *Service) DescribeStep(ctx context.Context, in *api.DescribeStepInput) (*api.DescribeStepOutput, error) {
	out := &api.DescribeStepOutput{}
	_, err := s.change(ctx, in.ClusterId, func(r *record) error {
		found := false
		for _, st := range r.Steps {
			if common.Value(in.StepId) == *st.Id {
				found = true
			}
		}

		if !found {
			return invalid("Step does not exist in this cluster.")
		}

		advance(r)
		for _, st := range r.Steps {
			if common.Value(in.StepId) == *st.Id {
				out.Step = common.Clone(&st)
			}
		}

		return nil
	})
	return out, err
}

func (s *Service) ListSteps(ctx context.Context, in *api.ListStepsInput) (*api.ListStepsOutput, error) {
	if err := checkStates(
		in.StepStates,
		strings.Fields("PENDING CANCEL_PENDING RUNNING COMPLETED CANCELLED FAILED INTERRUPTED"),
	); err != nil {
		return nil, err
	}

	r, err := s.change(ctx, in.ClusterId, func(r *record) error { advance(r); return nil })
	if err != nil {
		return nil, err
	}

	items := []api.StepSummary{}
	for _, st := range r.Steps {
		if len(in.StepStates) > 0 && !slices.Contains(in.StepStates, *st.Status.State) ||
			len(in.StepIds) > 0 && !slices.Contains(in.StepIds, *st.Id) {
			continue
		}

		items = append(
			items,
			api.StepSummary{
				Id:              st.Id,
				Name:            st.Name,
				ActionOnFailure: st.ActionOnFailure,
				Config:          st.Config,
				Status:          st.Status,
			},
		)
	}

	slices.SortFunc(items, func(a, b api.StepSummary) int { return strings.Compare(*a.Id, *b.Id) })
	out := &api.ListStepsOutput{}
	out.Steps, out.Marker, err = common.Page(
		items,
		func(st api.StepSummary) string { return *st.Id },
		in.Marker,
		50,
		invalid("Invalid Marker."),
	)
	return out, err
}

func (s *Service) CancelSteps(ctx context.Context, in *api.CancelStepsInput) (*api.CancelStepsOutput, error) {
	if len(in.StepIds) == 0 {
		return nil, invalid("StepIds is required.")
	}

	option := common.OrElse(in.StepCancellationOption, new("SEND_INTERRUPT"))
	if !slices.Contains([]string{"SEND_INTERRUPT", "TERMINATE_PROCESS"}, *option) {
		return nil, invalid("Invalid StepCancellationOption.")
	}

	out := &api.CancelStepsOutput{}
	_, err := s.change(ctx, in.ClusterId, func(r *record) error {
		for _, id := range in.StepIds {
			info := api.CancelStepsInfo{
				StepId: new(id),
				Status: new("FAILED"),
				Reason: new("Step does not exist or is terminal."),
			}

			for i := range r.Steps {
				st := &r.Steps[i]
				if *st.Id == id && slices.Contains([]string{pending, running}, *st.Status.State) {
					finishStep(st, cancelled)
					info.Status = new("SUBMITTED")
					info.Reason = new(simulated)
				}
			}

			out.CancelStepsInfoList = append(out.CancelStepsInfoList, info)
		}

		return nil
	})
	return out, err
}
