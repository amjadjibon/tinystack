package sfn

import (
	"context"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sfn"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// A redrive runs a failed, timed out, or aborted standard execution again
// from the top-level state where it stopped, with that state's input and the
// variables it saw, under the definition it ran. A Parallel or Map state runs
// again in full.

const (
	redriveWindow = 14 * 24 * time.Hour
	maxRedrives   = 1000
)

// resumePoint is the top-level state an execution entered last.
type resumePoint struct {
	Input any
	Vars  map[string]any
	State string
}

// redrivable reports why an execution cannot be redriven, or "" when it can.
func (e *execution) redrivable(now time.Time) string {
	switch {
	case e.HasMapRuns || e.MapRunARN != "":
		return "TinyStack does not support redriving distributed Map executions."
	case e.Status != statusFailed && e.Status != statusTimedOut && e.Status != statusAborted:
		return "Execution is " + e.Status + "."
	case e.Resume == nil || e.Definition == "":
		return "Execution has no state to redrive from."
	case now.Sub(time.Unix(0, int64(e.Stop*float64(time.Second)))) > redriveWindow:
		return "Execution stopped more than 14 days ago."
	case e.Redrives >= maxRedrives:
		return "Execution has been redriven 1000 times."
	}

	return ""
}

func (s *Service) RedriveExecution(
	ctx context.Context,
	in *api.RedriveExecutionInput,
) (*api.RedriveExecutionOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	now := s.now()
	var r *run
	var def *machine
	again := false
	var replayDate float64
	tokenKey := idempotency.Key(m.Scope, service, "RedriveExecution", in.ClientToken)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		date, replayed, replayErr := idempotency.Replay[float64](
			tx,
			tokenKey,
			in,
			awserror.New(
				"ExecutionNotRedrivable",
				"Execution not redrivable: ClientToken was used with different parameters.",
			),
		)
		if replayErr != nil {
			return replayErr
		}

		if replayed {
			replayDate, again = date, true
			return nil
		}

		e, loadErr := loadExecution(tx, &m, in.ExecutionArn)
		if loadErr != nil {
			return loadErr
		}

		if reason := e.redrivable(now); reason != "" {
			return awserror.New("ExecutionNotRedrivable", "Execution not redrivable: "+reason)
		}

		_, sm, machineErr := loadMachine(tx, &e.StateMachineARN)
		if machineErr != nil {
			return machineErr
		}

		ran := *sm
		ran.Definition = e.Definition
		def, _ = parseDefinition(e.Definition)
		e.Status, e.Error, e.Cause, e.Stop = statusRunning, "", "", 0
		e.Redrives, e.RedriveDate = e.Redrives+1, common.Unix(now)
		r = &run{
			s: s, machine: &ran, exec: e, scope: m.Scope, started: time.Unix(0, int64(e.Start*float64(time.Second))),
			record: true, lastEvent: e.Events, redrive: true,
		}

		if writeErr := storage.Write(tx, executionKey(m.Scope, e.StateMachine, e.Name), e); writeErr != nil {
			return writeErr
		}

		return idempotency.Remember(tx, tokenKey, in, e.RedriveDate)
	})
	if err != nil {
		return nil, err
	}

	if again {
		return &api.RedriveExecutionOutput{RedriveDate: new(replayDate)}, nil
	}

	if !again {
		r.useLogging()
		if !s.bg.Go(func(ctx context.Context) { s.execute(ctx, r, def) }) {
			return nil, awserror.New("ServiceException", "The service is shutting down")
		}
	}

	return &api.RedriveExecutionOutput{RedriveDate: new(r.exec.RedriveDate)}, nil
}
