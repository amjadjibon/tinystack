package batch

import (
	"context"
	"maps"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/batch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func queueOrder(
	tx storage.ReadTx,
	scope request.Scope,
	list []api.ComputeEnvironmentOrder,
	create bool,
) ([]api.ComputeEnvironmentOrder, error) {
	if len(list) > 3 || (create && len(list) < 1) {
		return nil, invalid("A new queue requires 1-3 compute environments; updates allow an empty association list")
	}

	out := slices.Clone(list)
	orders := map[int64]bool{}
	envs := map[string]bool{}
	var platform *bool
	for i, item := range out {
		if guardErr := integerOK(item.Order, 0, "order"); guardErr != nil {
			return nil, guardErr
		}

		if orders[*item.Order] {
			return nil, invalid("Duplicate compute environment order")
		}

		orders[*item.Order] = true
		r, err := read(tx, scope, kindCE, item.ComputeEnvironment)
		if err != nil {
			return nil, err
		}

		if envs[r.ID] {
			return nil, invalid("Duplicate compute environment")
		}

		envs[r.ID] = true
		if common.Value(r.CE.Status) != valid {
			return nil, invalid("Compute environments must be VALID")
		}

		fargate := r.CE.ComputeResources != nil && isFargate(common.Value(r.CE.ComputeResources.Type))
		if platform != nil && *platform != fargate {
			return nil, invalid("A queue cannot mix Fargate and EC2 environments")
		}

		platform = new(fargate)
		out[i].ComputeEnvironment = new(r.ARN)
	}

	slices.SortFunc(out, func(a, b api.ComputeEnvironmentOrder) int {
		if *a.Order < *b.Order {
			return -1
		}

		if *a.Order > *b.Order {
			return 1
		}

		return 0
	})
	return out, nil
}

func (s *Service) CreateJobQueue(
	ctx context.Context,
	in *api.CreateJobQueueRequest,
) (*api.CreateJobQueueResponse, error) {
	if guardErr := nameOK(in.JobQueueName); guardErr != nil {
		return nil, guardErr
	}

	if guardErr := stateOK(in.State); guardErr != nil {
		return nil, guardErr
	}

	if guardErr := integerOK(in.Priority, 0, "priority"); guardErr != nil {
		return nil, guardErr
	}

	if guardErr := tagsOK(in.Tags); guardErr != nil {
		return nil, guardErr
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	r := &record{ID: *in.JobQueueName, Kind: kindQueue, Tags: maps.Clone(in.Tags)}
	r.ARN = arn(scope, r.Kind, r.ID)
	r.Queue = &api.JobQueueDetail{
		JobQueueName: new(r.ID),
		JobQueueArn:  new(r.ARN),
		State:        common.OrElse(common.Clone(in.State), new(enabled)),
		Status:       new(valid),
		StatusReason: new(metadataReason),
		Priority:     common.Clone(in.Priority),
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r.Queue.ComputeEnvironmentOrder, e = queueOrder(tx, scope, in.ComputeEnvironmentOrder, true)
		if e != nil {
			return e
		}

		return unique(tx, scope, r)
	})
	return &api.CreateJobQueueResponse{JobQueueName: new(r.ID), JobQueueArn: new(r.ARN)}, err
}

func (s *Service) UpdateJobQueue(
	ctx context.Context,
	in *api.UpdateJobQueueRequest,
) (*api.UpdateJobQueueResponse, error) {
	if guardErr := stateOK(in.State); guardErr != nil {
		return nil, guardErr
	}

	if in.Priority != nil {
		if guardErr := integerOK(in.Priority, 0, "priority"); guardErr != nil {
			return nil, guardErr
		}
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.UpdateJobQueueResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, kindQueue, in.JobQueue)
		if e != nil {
			return e
		}

		if in.State != nil {
			r.Queue.State = common.Clone(in.State)
		}

		if in.Priority != nil {
			r.Queue.Priority = common.Clone(in.Priority)
		}

		if in.ComputeEnvironmentOrder != nil {
			r.Queue.ComputeEnvironmentOrder, e = queueOrder(tx, scope, in.ComputeEnvironmentOrder, false)
			if e != nil {
				return e
			}
		}

		out = &api.UpdateJobQueueResponse{JobQueueName: new(r.ID), JobQueueArn: new(r.ARN)}
		return save(tx, scope, r)
	})
	return out, err
}

func (s *Service) DeleteJobQueue(
	ctx context.Context,
	in *api.DeleteJobQueueRequest,
) (*api.DeleteJobQueueResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, kindQueue, in.JobQueue)
		if e != nil {
			return e
		}

		if common.Value(r.Queue.State) != disabled {
			return invalid("Disable the job queue before deleting it")
		}

		tx.Delete(scope.Key(serviceName, kindQueue, r.ID))
		return nil
	})
	return &api.DeleteJobQueueResponse{}, err
}

func (s *Service) DescribeJobQueues(
	ctx context.Context,
	in *api.DescribeJobQueuesRequest,
) (*api.DescribeJobQueuesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	refs, err := filters(scope, kindQueue, in.JobQueues, 100)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeJobQueuesResponse{JobQueues: []api.JobQueueDetail{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, e := rows(tx, scope, kindQueue)
		if e != nil {
			return e
		}

		if len(refs) > 0 {
			all = slices.DeleteFunc(all, func(r *record) bool { return !slices.Contains(refs, r.ID) })
		}

		all, out.NextToken, e = page(
			all,
			func(r *record) string { return r.ID },
			in.NextToken,
			in.MaxResults,
			scope,
			query(kindQueue, refs),
		)
		if e != nil {
			return e
		}

		for _, r := range all {
			out.JobQueues = append(out.JobQueues, queueOutput(r))
		}

		return nil
	})
	return out, err
}
