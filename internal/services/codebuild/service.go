// Package codebuild stores projects and optionally executes buildspecs in containers.
package codebuild

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codebuild"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName = "codebuild"
	projectKind = "project"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store     storage.Store
	runtime   Runtime
	caller    *catalog.Caller
	bg        *background.Group
	active    map[string]context.CancelFunc
	writeLogs func(context.Context, request.Scope, string, string, []string) error
	endpoint  string
	owner     string
	mu        sync.Mutex
}

func New(store storage.Store) *Service { return &Service{store: store} }
func invalid(message string) error     { return awserror.New("InvalidInputException", message) }
func missing() error {
	return awserror.New("ResourceNotFoundException", "Project does not exist in this account and Region")
}

func stamp() float64 { return float64(time.Now().UTC().UnixMicro()) / 1e6 }
func arn(scope request.Scope, name string) string {
	return fmt.Sprintf("arn:aws:codebuild:%s:%s:project/%s", scope.Region, scope.AccountID, name)
}

func read(tx storage.ReadTx, scope request.Scope, name string) (*api.Project, bool, error) {
	return storage.Read[api.Project](tx, scope.Key(serviceName, projectKind, name))
}

func save(tx storage.WriteTx, scope request.Scope, p *api.Project) error {
	return storage.Write(tx, scope.Key(serviceName, projectKind, *p.Name), p)
}

func (s *Service) CreateProject(ctx context.Context, in *api.CreateProjectInput) (*api.CreateProjectOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}

	p := common.Clone(&api.Project{
		Name:                   in.Name,
		Description:            in.Description,
		Source:                 in.Source,
		SourceVersion:          in.SourceVersion,
		Artifacts:              in.Artifacts,
		Environment:            in.Environment,
		ServiceRole:            in.ServiceRole,
		TimeoutInMinutes:       in.TimeoutInMinutes,
		QueuedTimeoutInMinutes: in.QueuedTimeoutInMinutes,
		Tags:                   in.Tags,
		LogsConfig:             in.LogsConfig,
	})
	if err := validate(scope, p); err != nil {
		return nil, err
	}

	p.Arn = new(arn(scope, *p.Name))
	p.Created = new(stamp())
	p.LastModified = new(*p.Created)
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, ok, err := read(tx, scope, *p.Name)
		if err != nil {
			return err
		}

		if ok {
			return awserror.New("ResourceAlreadyExistsException", "Project name already exists")
		}

		return save(tx, scope, p)
	})
	if e != nil {
		return nil, e
	}

	return &api.CreateProjectOutput{Project: common.Clone(p)}, nil
}

func (s *Service) UpdateProject(ctx context.Context, in *api.UpdateProjectInput) (*api.UpdateProjectOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}

	if err := nameOK(in.Name); err != nil {
		return nil, err
	}

	var result *api.Project
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, ok, err := read(tx, scope, *in.Name)
		if err != nil {
			return err
		}

		if !ok {
			return missing()
		}

		if in.LogsConfig != nil {
			p.LogsConfig = in.LogsConfig
		}

		if in.Description != nil {
			p.Description = in.Description
		}

		if in.Source != nil {
			p.Source = in.Source
		}

		if in.SourceVersion != nil {
			p.SourceVersion = in.SourceVersion
		}

		if in.Artifacts != nil {
			p.Artifacts = in.Artifacts
		}

		if in.Environment != nil {
			p.Environment = in.Environment
		}

		if in.ServiceRole != nil {
			p.ServiceRole = in.ServiceRole
		}

		if in.TimeoutInMinutes != nil {
			p.TimeoutInMinutes = in.TimeoutInMinutes
		}

		if in.QueuedTimeoutInMinutes != nil {
			p.QueuedTimeoutInMinutes = in.QueuedTimeoutInMinutes
		}

		if in.Tags != nil {
			p.Tags = in.Tags
		}

		// Own every pointer before applying defaults; inputs and earlier outputs remain immutable.
		p = common.Clone(p)
		if err := validate(scope, p); err != nil {
			return err
		}

		now := stamp()
		if now <= common.Value(p.LastModified) {
			now = common.Value(p.LastModified) + 0.000001
		}

		p.LastModified = new(now)
		if err := save(tx, scope, p); err != nil {
			return err
		}

		result = common.Clone(p)
		return nil
	})
	if e != nil {
		return nil, e
	}

	return &api.UpdateProjectOutput{Project: result}, nil
}

func (s *Service) DeleteProject(ctx context.Context, in *api.DeleteProjectInput) (*api.DeleteProjectOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}

	if err := nameOK(in.Name); err != nil {
		return nil, err
	}

	e = s.store.Update(
		ctx,
		func(tx storage.WriteTx) error { tx.Delete(scope.Key(serviceName, projectKind, *in.Name)); return nil },
	)
	if e != nil {
		return nil, e
	}

	return &api.DeleteProjectOutput{}, nil
}

func (s *Service) BatchGetProjects(
	ctx context.Context,
	in *api.BatchGetProjectsInput,
) (*api.BatchGetProjectsOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}

	if len(in.Names) < 1 || len(in.Names) > 100 {
		return nil, invalid("Names must contain 1-100 project names or ARNs")
	}

	type lookup struct {
		name, original string
		local          bool
	}

	lookups := make([]lookup, 0, len(in.Names))
	for _, v := range in.Names {
		name, local, err := projectIdentifier(scope, v)
		if err != nil {
			return nil, err
		}

		lookups = append(lookups, lookup{name, v, local})
	}

	out := &api.BatchGetProjectsOutput{Projects: []api.Project{}, ProjectsNotFound: []string{}}
	e = s.store.View(ctx, func(tx storage.ReadTx) error {
		seen := map[string]bool{}
		for _, v := range lookups {
			if seen[v.original] {
				continue
			}

			seen[v.original] = true
			if !v.local {
				out.ProjectsNotFound = append(out.ProjectsNotFound, v.original)
				continue
			}

			p, ok, err := read(tx, scope, v.name)
			if err != nil {
				return err
			}

			if !ok {
				out.ProjectsNotFound = append(out.ProjectsNotFound, v.original)
			} else {
				out.Projects = append(out.Projects, *common.Clone(p))
			}
		}

		return nil
	})
	if e != nil {
		return nil, e
	}

	return out, nil
}

func tagMap(tags []api.Tag) map[string]string {
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		out[common.Value(t.Key)] = common.Value(t.Value)
	}

	return out
}

func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
}

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	e := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, kind, ok := storage.Scoped(key, serviceName)
			if !ok || !strings.HasPrefix(kind, projectKind+"/") {
				continue
			}

			p, _, err := storage.Read[api.Project](tx, key)
			if err != nil {
				return err
			}

			out = append(out, catalog.Resource{
				Type:    projectKind,
				Name:    *p.Name,
				ARN:     *p.Arn,
				Account: account,
				Region:  region,
				Tags:    tagMap(p.Tags),
				Details: map[string]any{
					"RecordsOnly":    s.runtime == nil,
					"ExecutionReady": s.runtime != nil,
					"Project":        common.Clone(p),
				},
			})
		}

		return nil
	})
	return out, e
}
