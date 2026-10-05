package sfn

import (
	"context"
	"encoding/json"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sfn"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// pollWait is how long GetActivityTask waits for a task.
var pollWait = 60 * time.Second

func loadActivity(tx storage.ReadTx, m *request.Metadata, arnString *string) (*activity, error) {
	scope, kind, name, ok := parseARN(common.Value(arnString))
	if !ok || kind != kindActivity {
		return nil, invalidARN(common.Value(arnString))
	}

	if err := inScope(m, scope, arnString); err != nil {
		return nil, err
	}

	a, found, err := storage.Read[activity](tx, activityKey(scope, name))
	if err != nil || !found {
		if err == nil {
			err = awserror.New("ActivityDoesNotExist", "Activity Does Not Exist: '"+common.Value(arnString)+"'")
		}

		return nil, err
	}

	return a, nil
}

// CreateActivity creates an activity, or returns the one with the name.
func (s *Service) CreateActivity(ctx context.Context, in *api.CreateActivityInput) (*api.CreateActivityOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.Name)
	if checkErr := checkName(name); checkErr != nil {
		return nil, checkErr
	}

	tags := tagsOf(in.Tags)
	if checkErr := checkTags(tags); checkErr != nil {
		return nil, checkErr
	}

	a := &activity{Name: name, Tags: tags, Created: common.Unix(s.now())}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		existing, found, readErr := storage.Read[activity](tx, activityKey(m.Scope, name))
		if readErr != nil || found {
			if found {
				a = existing
			}

			return readErr
		}

		return storage.Write(tx, activityKey(m.Scope, name), a)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateActivityOutput{
		ActivityArn:  new(arn(m.Scope, kindActivity, name)),
		CreationDate: new(a.Created),
	}, nil
}

func (s *Service) DescribeActivity(
	ctx context.Context,
	in *api.DescribeActivityInput,
) (*api.DescribeActivityOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var a *activity
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		a, loadErr = loadActivity(tx, &m, in.ActivityArn)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeActivityOutput{
		ActivityArn:  in.ActivityArn,
		Name:         new(a.Name),
		CreationDate: new(a.Created),
	}, nil
}

func (s *Service) ListActivities(ctx context.Context, in *api.ListActivitiesInput) (*api.ListActivitiesOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListActivitiesOutput{Activities: []api.ActivityListItem{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, readErr := recordsOf[activity](tx, prefix(m.Scope, kindActivity))
		if readErr != nil {
			return readErr
		}

		items := make([]api.ActivityListItem, 0, len(all))
		for _, a := range all {
			items = append(items, api.ActivityListItem{
				ActivityArn: new(arn(m.Scope, kindActivity, a.Name)), Name: new(a.Name), CreationDate: new(a.Created),
			})
		}

		items, out.NextToken, readErr = page(items, func(i api.ActivityListItem) string { return *i.Name },
			in.NextToken, in.MaxResults)
		out.Activities = append(out.Activities, items...)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteActivity(ctx context.Context, in *api.DeleteActivityInput) (*api.DeleteActivityOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadActivity(tx, &m, in.ActivityArn)
		if loadErr != nil {
			return loadErr
		}

		tx.Delete(activityKey(m.Scope, a.Name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteActivityOutput{}, nil
}

// GetActivityTask waits up to a minute for a task scheduled on the activity.
// With none, it returns no token.
func (s *Service) GetActivityTask(
	ctx context.Context,
	in *api.GetActivityTaskInput,
) (*api.GetActivityTaskOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, loadErr := loadActivity(tx, &m, in.ActivityArn)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	timer := time.NewTimer(pollWait)
	defer timer.Stop()
	for {
		select {
		case task := <-s.queue(*in.ActivityArn):
			s.mu.Lock()
			w, ok := s.tokens[task.token]
			s.mu.Unlock()
			if !ok {
				continue // The execution stopped waiting.
			}

			select {
			case w.started <- common.Value(in.WorkerName):
			default:
			}

			return &api.GetActivityTaskOutput{TaskToken: new(task.token), Input: new(task.input)}, nil
		case <-timer.C:
			return &api.GetActivityTaskOutput{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.ctx.Done():
			return &api.GetActivityTaskOutput{}, nil
		}
	}
}

// resolve delivers a task token's outcome.
func (s *Service) resolve(token *string, o outcome) error {
	s.mu.Lock()
	w, ok := s.tokens[common.Value(token)]
	s.mu.Unlock()
	if !ok {
		return awserror.New("TaskDoesNotExist", "Task Does Not Exist")
	}

	select {
	case w.done <- o:
		return nil
	default:
		return awserror.New("TaskDoesNotExist", "Task Does Not Exist")
	}
}

func (s *Service) SendTaskSuccess(_ context.Context, in *api.SendTaskSuccessInput) (*api.SendTaskSuccessOutput, error) {
	var output any
	if err := json.Unmarshal([]byte(common.Value(in.Output)), &output); err != nil {
		return nil, awserror.New("InvalidOutput", "Invalid Output: the output must be JSON")
	}

	if err := s.resolve(in.TaskToken, outcome{output: output}); err != nil {
		return nil, err
	}

	return &api.SendTaskSuccessOutput{}, nil
}

func (s *Service) SendTaskFailure(_ context.Context, in *api.SendTaskFailureInput) (*api.SendTaskFailureOutput, error) {
	se := &stateError{Name: common.Value(in.Error), Cause: common.Value(in.Cause)}
	if err := s.resolve(in.TaskToken, outcome{err: se}); err != nil {
		return nil, err
	}

	return &api.SendTaskFailureOutput{}, nil
}

func (s *Service) SendTaskHeartbeat(
	_ context.Context,
	in *api.SendTaskHeartbeatInput,
) (*api.SendTaskHeartbeatOutput, error) {
	s.mu.Lock()
	w, ok := s.tokens[common.Value(in.TaskToken)]
	s.mu.Unlock()
	if !ok {
		return nil, awserror.New("TaskDoesNotExist", "Task Does Not Exist")
	}

	select {
	case w.heartbeat <- struct{}{}:
	default:
	}

	return &api.SendTaskHeartbeatOutput{}, nil
}
