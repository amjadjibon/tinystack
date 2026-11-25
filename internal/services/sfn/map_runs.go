package sfn

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sfn"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const kindMapRun = "maprun"

type mapCounts struct {
	Pending               int64
	Running               int64
	Succeeded             int64
	Failed                int64
	TimedOut              int64
	Aborted               int64
	Total                 int64
	ResultsWritten        int64
	FailuresNotRedrivable int64
	PendingRedrive        int64
}

type mapJobState struct {
	Input           any     `json:",omitempty"`
	ChildARN        string  `json:",omitempty"`
	StateMachineARN string  `json:",omitempty"`
	Name            string  `json:",omitempty"`
	Status          string  `json:",omitempty"`
	Output          string  `json:",omitempty"`
	Error           string  `json:",omitempty"`
	Cause           string  `json:",omitempty"`
	Start           float64 `json:",omitempty"`
	Stop            float64 `json:",omitempty"`
	Count           int64   `json:",omitempty"`
}

func (j *mapJobState) toExecution(mapRunARN string) *execution {
	return &execution{
		ARN:             j.ChildARN,
		StateMachineARN: j.StateMachineARN,
		Name:            j.Name,
		MapRunARN:       mapRunARN,
		Status:          j.Status,
		Input:           *text(j.Input),
		Output:          j.Output,
		Error:           j.Error,
		Cause:           j.Cause,
		Start:           j.Start,
		Stop:            j.Stop,
		ItemCount:       j.Count,
	}
}

func jobStateFromExec(input any, count int64, child *execution) mapJobState {
	js := mapJobState{Input: input, Count: count}
	if child != nil {
		js.ChildARN = child.ARN
		js.StateMachineARN = child.StateMachineARN
		js.Name = child.Name
		js.Status = child.Status
		js.Output = child.Output
		js.Error = child.Error
		js.Cause = child.Cause
		js.Start = child.Start
		js.Stop = child.Stop
	}

	return js
}

type mapRun struct {
	ARN                        string
	ExecutionARN               string
	StateMachineARN            string
	ChildMachine               string
	Status                     string
	Jobs                       []mapJobState `json:",omitempty"`
	Start                      float64
	Stop                       float64
	RedriveDate                float64 `json:",omitempty"`
	MaxConcurrency             int64
	ToleratedFailureCount      int64
	ToleratedFailurePercentage float64
	RedriveCount               int64 `json:",omitempty"`
	Items                      mapCounts
	Executions                 mapCounts
	// A threshold defaults to zero only when neither threshold was specified.
	CountThreshold      bool
	PercentageThreshold bool
}

func loadMapRun(tx storage.ReadTx, m *request.Metadata, value *string) (*mapRun, error) {
	a := common.Value(value)
	scope, kind, name, ok := parseARN(a)
	if !ok || kind != "mapRun" || !strings.Contains(name, ":") {
		return nil, invalidARN(a)
	}

	if err := inScope(m, scope, value); err != nil {
		return nil, err
	}

	record, found, err := storage.Read[mapRun](tx, scope.Key(service, kindMapRun, name))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, awserror.New("ResourceNotFound", "Map Run does not exist: "+a)
	}

	return record, nil
}

func mapRunKey(a string) string {
	scope, _, name, _ := parseARN(a)
	return scope.Key(service, kindMapRun, name)
}

func (s *Service) DescribeMapRun(ctx context.Context, in *api.DescribeMapRunInput) (*api.DescribeMapRunOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var mr *mapRun
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		mr, readErr = loadMapRun(tx, new(m), in.MapRunArn)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	i, x := mr.Items, mr.Executions
	out := &api.DescribeMapRunOutput{
		MapRunArn: new(mr.ARN), ExecutionArn: new(mr.ExecutionARN), Status: new(mr.Status),
		StartDate: new(mr.Start), MaxConcurrency: new(mr.MaxConcurrency),
		ToleratedFailureCount:      new(mr.ToleratedFailureCount),
		ToleratedFailurePercentage: new(mr.ToleratedFailurePercentage), RedriveCount: new(mr.RedriveCount),
		ItemCounts: &api.MapRunItemCounts{
			Pending:   new(i.Pending),
			Running:   new(i.Running),
			Succeeded: new(i.Succeeded),
			Failed:    new(i.Failed),
			TimedOut: new(
				i.TimedOut,
			),
			Aborted:               new(i.Aborted),
			Total:                 new(i.Total),
			ResultsWritten:        new(i.ResultsWritten),
			FailuresNotRedrivable: new(i.FailuresNotRedrivable),
			PendingRedrive:        new(i.PendingRedrive),
		},
		ExecutionCounts: &api.MapRunExecutionCounts{
			Pending:   new(x.Pending),
			Running:   new(x.Running),
			Succeeded: new(x.Succeeded),
			Failed:    new(x.Failed),
			TimedOut: new(
				x.TimedOut,
			),
			Aborted:               new(x.Aborted),
			Total:                 new(x.Total),
			ResultsWritten:        new(x.ResultsWritten),
			FailuresNotRedrivable: new(x.FailuresNotRedrivable),
			PendingRedrive:        new(x.PendingRedrive),
		},
	}
	if mr.RedriveCount > 0 {
		out.RedriveDate = new(mr.RedriveDate)
	}

	if mr.Status != statusRunning {
		out.StopDate = new(mr.Stop)
	}

	return out, nil
}

func (s *Service) ListMapRuns(ctx context.Context, in *api.ListMapRunsInput) (*api.ListMapRunsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListMapRunsOutput{MapRuns: []api.MapRunListItem{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if _, loadErr := loadExecution(tx, new(m), in.ExecutionArn); loadErr != nil {
			return loadErr
		}

		records, readErr := storage.Records[mapRun](tx, m.Prefix(service, kindMapRun))
		if readErr != nil {
			return readErr
		}

		var items []api.MapRunListItem
		for _, mr := range records {
			if mr.ExecutionARN != common.Value(in.ExecutionArn) {
				continue
			}

			item := api.MapRunListItem{
				MapRunArn: new(mr.ARN), ExecutionArn: new(mr.ExecutionARN),
				StateMachineArn: new(mr.StateMachineARN), StartDate: new(mr.Start),
			}

			if mr.Status != statusRunning {
				item.StopDate = new(mr.Stop)
			}

			items = append(items, item)
		}

		var pageErr error
		out.MapRuns, out.NextToken, pageErr = page(items, func(i api.MapRunListItem) string {
			return common.Value(i.MapRunArn)
		}, in.NextToken, in.MaxResults)
		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateMapRun(ctx context.Context, in *api.UpdateMapRunInput) (*api.UpdateMapRunOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		mr, loadErr := loadMapRun(tx, new(m), in.MapRunArn)
		if loadErr != nil {
			return loadErr
		}

		if mr.Status != statusRunning {
			return awserror.Validation("Map Run is not running")
		}

		if in.MaxConcurrency != nil {
			mr.MaxConcurrency = *in.MaxConcurrency
		}

		if in.ToleratedFailureCount != nil {
			mr.ToleratedFailureCount, mr.CountThreshold = *in.ToleratedFailureCount, true
		}

		if in.ToleratedFailurePercentage != nil {
			mr.ToleratedFailurePercentage, mr.PercentageThreshold = *in.ToleratedFailurePercentage, true
		}

		return storage.Write(tx, mapRunKey(mr.ARN), mr)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateMapRunOutput{}, nil
}

func (mr *mapRun) exceedsThreshold() bool {
	failed := mr.Items.Failed + mr.Items.TimedOut
	if !mr.CountThreshold && !mr.PercentageThreshold {
		return failed > 0
	}

	if mr.CountThreshold && failed > mr.ToleratedFailureCount {
		return true
	}

	return mr.PercentageThreshold && mr.Items.Total > 0 &&
		float64(failed)*100/float64(mr.Items.Total) > mr.ToleratedFailurePercentage
}

func readMapRun(tx storage.ReadTx, a string) (*mapRun, error) {
	record, found, err := storage.Read[mapRun](tx, mapRunKey(a))
	if err == nil && !found {
		err = awserror.New("ResourceNotFound", "Map Run does not exist")
	}

	return record, err
}

// currentMapRun reads a running Map Run, so a dispatcher sees UpdateMapRun changes without writing.
func (s *Service) currentMapRun(ctx context.Context, a string) (*mapRun, error) {
	var mr *mapRun
	err := s.store.View(context.WithoutCancel(ctx), func(tx storage.ReadTx) error {
		var readErr error
		mr, readErr = readMapRun(tx, a)
		return readErr
	})
	return mr, err
}

func (s *Service) changeMapRun(ctx context.Context, a string, change func(*mapRun)) (*mapRun, error) {
	var mr *mapRun
	err := s.store.Update(context.WithoutCancel(ctx), func(tx storage.WriteTx) error {
		record, readErr := readMapRun(tx, a)
		if readErr != nil {
			return readErr
		}

		change(record)
		mr = record
		return storage.Write(tx, mapRunKey(a), record)
	})
	return mr, err
}
