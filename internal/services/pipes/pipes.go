package pipes

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/pipes"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagError(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return awserror.New("ServiceQuotaExceededException", err.Error())
	}

	return awserror.Validation("%s", err.Error())
}

// index keeps a pipe in the runner's index while it should be running.
func index(tx storage.WriteTx, scope request.Scope, p *pipe) {
	if p.DesiredState == stateRunning {
		tx.Put(indexKey(scope, p.Name), []byte("{}"))
		return
	}

	tx.Delete(indexKey(scope, p.Name))
}

// summary is the response of the operations that change a pipe.
func summary(scope request.Scope, p *pipe, current string) *api.CreatePipeResponse {
	return &api.CreatePipeResponse{
		Arn: new(pipeARN(scope, p.Name)), Name: new(p.Name), DesiredState: new(p.DesiredState),
		CurrentState: new(current), CreationTime: new(p.Created), LastModifiedTime: new(p.Modified),
	}
}

func (s *Service) CreatePipe(ctx context.Context, in *api.CreatePipeRequest) (*api.CreatePipeResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	now := common.Unix(s.now())
	p := &pipe{
		Name: common.Value(in.Name), Description: common.Value(in.Description),
		DesiredState: common.Value(common.OrElse(in.DesiredState, new(stateRunning))),
		Source:       common.Value(in.Source), SourceParameters: common.Clone(in.SourceParameters),
		Enrichment: common.Value(in.Enrichment), EnrichmentParameters: common.Clone(in.EnrichmentParameters),
		Target: common.Value(in.Target), TargetParameters: common.Clone(in.TargetParameters),
		RoleArn: common.Value(in.RoleArn), Tags: common.CloneTags(in.Tags), Created: now, Modified: now,
	}
	if err = tagging.CheckSet(p.Tags); err != nil {
		return nil, tagError(err)
	}

	if checkErr := s.check(ctx, m.Scope, p, true); checkErr != nil {
		return nil, checkErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := m.Key(service, kindPipe, p.Name)
		if _, found := tx.Get(key); found {
			return awserror.New("ConflictException", "Pipe "+p.Name+" already exists.")
		}

		index(tx, m.Scope, p)

		return storage.Write(tx, key, p)
	})
	if err != nil {
		return nil, err
	}

	return summary(m.Scope, p, stateCreating), nil
}

// load reads a pipe in a transaction.
func load(tx storage.ReadTx, scope request.Scope, name string) (*pipe, error) {
	p, found, err := storage.Read[pipe](tx, scope.Key(service, kindPipe, name))
	if err == nil && !found {
		err = notFound(name)
	}

	return p, err
}

func (s *Service) DescribePipe(ctx context.Context, in *api.DescribePipeRequest) (*api.DescribePipeResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var p *pipe
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p, err = load(tx, m.Scope, common.Value(in.Name))
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribePipeResponse{
		Arn: new(pipeARN(m.Scope, p.Name)), Name: new(p.Name), Description: new(p.Description),
		DesiredState: new(p.DesiredState), CurrentState: new(p.currentState()), StateReason: new("User initiated"),
		Source: new(p.Source), SourceParameters: p.SourceParameters, Enrichment: new(p.Enrichment),
		EnrichmentParameters: p.EnrichmentParameters, Target: new(p.Target), TargetParameters: p.TargetParameters,
		RoleArn: new(p.RoleArn), Tags: p.Tags, CreationTime: new(p.Created), LastModifiedTime: new(p.Modified),
	}, nil
}

// merge applies an UpdatePipe request's source parameters: each block the
// request holds replaces the pipe's, keeping the settings (such as a
// starting position) it cannot change.
func merge(current *api.PipeSourceParameters, update *api.UpdatePipeSourceParameters) *api.PipeSourceParameters {
	if update == nil {
		return current
	}

	out := &api.PipeSourceParameters{}
	if current != nil {
		*out = *common.Clone(current)
	}

	if update.FilterCriteria != nil {
		out.FilterCriteria = update.FilterCriteria
	}

	if u := update.SqsQueueParameters; u != nil {
		q := common.Value(out.SqsQueueParameters)
		q.BatchSize, q.MaximumBatchingWindowInSeconds = common.OrElse(u.BatchSize, q.BatchSize),
			common.OrElse(u.MaximumBatchingWindowInSeconds, q.MaximumBatchingWindowInSeconds)
		out.SqsQueueParameters = &q
	}

	if u := update.KinesisStreamParameters; u != nil {
		k := common.Value(out.KinesisStreamParameters)
		k.BatchSize, k.MaximumBatchingWindowInSeconds = common.OrElse(u.BatchSize, k.BatchSize),
			common.OrElse(u.MaximumBatchingWindowInSeconds, k.MaximumBatchingWindowInSeconds)
		out.KinesisStreamParameters = &k
	}

	if u := update.DynamoDBStreamParameters; u != nil {
		d := common.Value(out.DynamoDBStreamParameters)
		d.BatchSize, d.MaximumBatchingWindowInSeconds = common.OrElse(u.BatchSize, d.BatchSize),
			common.OrElse(u.MaximumBatchingWindowInSeconds, d.MaximumBatchingWindowInSeconds)
		out.DynamoDBStreamParameters = &d
	}

	return out
}

func (s *Service) UpdatePipe(ctx context.Context, in *api.UpdatePipeRequest) (*api.UpdatePipeResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.Name)
	var p *pipe
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p, err = load(tx, m.Scope, name)
		return err
	})
	if err != nil {
		return nil, err
	}

	if in.Description != nil {
		p.Description = *in.Description
	}

	if in.DesiredState != nil {
		p.DesiredState = *in.DesiredState
	}

	if in.Enrichment != nil {
		p.Enrichment = *in.Enrichment
	}

	if in.EnrichmentParameters != nil {
		p.EnrichmentParameters = common.Clone(in.EnrichmentParameters)
	}

	if in.Target != nil {
		p.Target = *in.Target
	}

	if in.TargetParameters != nil {
		p.TargetParameters = common.Clone(in.TargetParameters)
	}

	if in.RoleArn != nil {
		p.RoleArn = *in.RoleArn
	}

	p.SourceParameters = merge(p.SourceParameters, in.SourceParameters)
	p.Modified = common.Unix(s.now())
	if checkErr := s.check(ctx, m.Scope, p, false); checkErr != nil {
		return nil, checkErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		// The runner may have moved the cursors since the pipe was read.
		current, loadErr := load(tx, m.Scope, name)
		if loadErr != nil {
			return loadErr
		}

		p.Cursor, p.Cursors, p.Tags = current.Cursor, current.Cursors, current.Tags
		index(tx, m.Scope, p)

		return storage.Write(tx, m.Key(service, kindPipe, name), p)
	})
	if err != nil {
		return nil, err
	}

	return (*api.UpdatePipeResponse)(summary(m.Scope, p, stateUpdating)), nil
}

func (s *Service) DeletePipe(ctx context.Context, in *api.DeletePipeRequest) (*api.DeletePipeResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var p *pipe
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if p, err = load(tx, m.Scope, common.Value(in.Name)); err != nil {
			return err
		}

		tx.Delete(m.Key(service, kindPipe, p.Name))
		tx.Delete(indexKey(m.Scope, p.Name))

		return nil
	})
	if err != nil {
		return nil, err
	}

	s.checked.Delete(pipeARN(m.Scope, p.Name))
	out := summary(m.Scope, p, stateDeleting)
	out.DesiredState = new(stateDeleted)

	return (*api.DeletePipeResponse)(out), nil
}

// setState starts or stops a pipe. Doing either to a pipe that is already
// there succeeds.
func (s *Service) setState(ctx context.Context, name *string, desired, transition string) (
	*api.CreatePipeResponse, error,
) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var p *pipe
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if p, err = load(tx, m.Scope, common.Value(name)); err != nil {
			return err
		}

		if p.DesiredState == desired {
			return nil
		}

		p.DesiredState, p.Modified = desired, common.Unix(s.now())
		index(tx, m.Scope, p)

		return storage.Write(tx, m.Key(service, kindPipe, p.Name), p)
	})
	if err != nil {
		return nil, err
	}

	return summary(m.Scope, p, transition), nil
}

func (s *Service) StartPipe(ctx context.Context, in *api.StartPipeRequest) (*api.StartPipeResponse, error) {
	out, err := s.setState(ctx, in.Name, stateRunning, stateStarting)
	return (*api.StartPipeResponse)(out), err
}

func (s *Service) StopPipe(ctx context.Context, in *api.StopPipeRequest) (*api.StopPipeResponse, error) {
	out, err := s.setState(ctx, in.Name, stateStopped, stateStopping)
	return (*api.StopPipeResponse)(out), err
}

func (s *Service) ListPipes(ctx context.Context, in *api.ListPipesRequest) (*api.ListPipesResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	limit := int(common.Value(common.OrElse(in.Limit, new(int64(defaultLimit)))))
	if limit < 1 || limit > defaultLimit {
		return nil, awserror.Validation("Limit must be between 1 and %d", defaultLimit)
	}

	var pipes []*pipe
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		pipes, err = storage.Records[pipe](tx, m.Prefix(service, kindPipe))
		return err
	})
	if err != nil {
		return nil, err
	}

	pipes = slices.DeleteFunc(pipes, func(p *pipe) bool {
		return !strings.HasPrefix(p.Name, common.Value(in.NamePrefix)) ||
			in.DesiredState != nil && p.DesiredState != *in.DesiredState ||
			in.CurrentState != nil && p.currentState() != *in.CurrentState ||
			!strings.HasPrefix(p.Source, common.Value(in.SourcePrefix)) ||
			!strings.HasPrefix(p.Target, common.Value(in.TargetPrefix))
	})
	slices.SortFunc(pipes, func(a, b *pipe) int { return strings.Compare(a.Name, b.Name) })

	page, next, err := common.Page(pipes, func(p *pipe) string { return p.Name }, in.NextToken, limit,
		awserror.Validation("Invalid NextToken"))
	if err != nil {
		return nil, err
	}

	out := &api.ListPipesResponse{Pipes: make([]api.Pipe, 0, len(page)), NextToken: next}
	for _, p := range page {
		out.Pipes = append(out.Pipes, api.Pipe{
			Name: new(p.Name), Arn: new(pipeARN(m.Scope, p.Name)), DesiredState: new(p.DesiredState),
			CurrentState: new(p.currentState()), StateReason: new("User initiated"), CreationTime: new(p.Created),
			LastModifiedTime: new(p.Modified), Source: new(p.Source), Target: new(p.Target),
			Enrichment: new(p.Enrichment),
		})
	}

	return out, nil
}

// taggedPipe returns the name of the pipe an ARN names.
func taggedPipe(scope request.Scope, arn *string) (string, error) {
	name, ok := strings.CutPrefix(common.Value(arn), pipeARN(scope, ""))
	if !ok || name == "" {
		return "", notFound(common.Value(arn))
	}

	return name, nil
}

func (s *Service) retag(ctx context.Context, arn *string, change func(map[string]string) (map[string]string, error),
) error {
	m, err := request.Regional(ctx)
	if err != nil {
		return err
	}

	name, err := taggedPipe(m.Scope, arn)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, loadErr := load(tx, m.Scope, name)
		if loadErr != nil {
			return loadErr
		}

		if p.Tags, loadErr = change(p.Tags); loadErr != nil {
			return loadErr
		}

		return storage.Write(tx, m.Key(service, kindPipe, name), p)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	if err := tagging.CheckSet(in.Tags); err != nil {
		return nil, tagError(err)
	}

	err := s.retag(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string, error) {
		merged, err := tagging.Merge(tags, in.Tags)
		if err != nil {
			return nil, tagError(err)
		}

		return merged, nil
	})

	return &api.TagResourceResponse{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string, error) {
		return tagging.Remove(tags, in.TagKeys), nil
	})

	return &api.UntagResourceResponse{}, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context, in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name, err := taggedPipe(m.Scope, in.ResourceArn)
	if err != nil {
		return nil, err
	}

	var p *pipe
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p, err = load(tx, m.Scope, name)
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{Tags: common.CloneTags(p.Tags)}, nil
}
