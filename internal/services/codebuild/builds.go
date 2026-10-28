package codebuild

import (
	"context"
	"fmt"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/codebuild"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	buildKind  = "build"
	buildIndex = "aws/codebuild/index/"
)

type buildRecord struct {
	Build    api.Build
	Project  api.Project
	Scope    request.Scope
	Endpoint string
}

func indexKey(scope request.Scope, id string) string {
	return buildIndex + scope.AccountID + "/" + scope.Region + "/" + id
}

func (s *Service) StartBuild(ctx context.Context, in *api.StartBuildInput) (*api.StartBuildOutput, error) {
	meta, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if s.runtime == nil {
		return nil, invalid("StartBuild requires codebuild.builds: docker; records mode cannot execute builds")
	}

	name, local, err := projectIdentifier(meta.Scope, common.Value(in.ProjectName))
	if err != nil {
		return nil, err
	}

	if !local {
		return nil, missing()
	}

	var result *api.Build
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, ok, e := read(tx, meta.Scope, name)
		if e != nil {
			return e
		}

		if !ok {
			return missing()
		}

		p = common.Clone(p)
		if source, ok := ctx.Value(pipelineSourceKey{}).(string); ok {
			if common.Value(p.Source.Type) != pipelineType {
				return invalid("CodePipeline builds require a CODEPIPELINE project source")
			}

			p.Source.Type = new(s3Type)
			p.Source.Location = new(source)
			p.SourceVersion = nil
		}

		if in.SourceVersion != nil {
			p.SourceVersion = in.SourceVersion
		}

		if in.BuildspecOverride != nil {
			p.Source.Buildspec = in.BuildspecOverride
		}

		if in.ImageOverride != nil {
			p.Environment.Image = in.ImageOverride
		}

		if in.PrivilegedModeOverride != nil {
			p.Environment.PrivilegedMode = in.PrivilegedModeOverride
		}

		if in.TimeoutInMinutesOverride != nil {
			p.TimeoutInMinutes = in.TimeoutInMinutesOverride
		}

		if in.ArtifactsOverride != nil {
			p.Artifacts = common.Clone(in.ArtifactsOverride)
		}

		if in.LogsConfigOverride != nil {
			p.LogsConfig = common.Clone(in.LogsConfigOverride)
		}

		overrideEnv := common.Clone(p.Environment)
		overrideEnv.EnvironmentVariables = append([]api.EnvironmentVariable{}, in.EnvironmentVariablesOverride...)
		if validationErr := environmentOK(overrideEnv); validationErr != nil {
			return validationErr
		}

		vars := map[string]api.EnvironmentVariable{}
		for _, v := range in.EnvironmentVariablesOverride {
			vars[common.Value(v.Name)] = v
		}

		for i, v := range p.Environment.EnvironmentVariables {
			if replacement, exists := vars[common.Value(v.Name)]; exists {
				p.Environment.EnvironmentVariables[i] = replacement
				delete(vars, common.Value(v.Name))
			}
		}

		for _, v := range in.EnvironmentVariablesOverride {
			if _, exists := vars[common.Value(v.Name)]; exists {
				p.Environment.EnvironmentVariables = append(p.Environment.EnvironmentVariables, v)
				delete(vars, common.Value(v.Name))
			}
		}

		if validationErr := validate(meta.Scope, p); validationErr != nil {
			return validationErr
		}

		switch common.Value(p.Source.Type) {
		case noSource, s3Type:
		default:
			return invalid(
				"Build execution supports NO_SOURCE and S3 only; GitHub fetches and CodeCommit git content are unavailable",
			)
		}

		if strings.HasPrefix(common.Value(p.Source.Buildspec), "arn:") {
			return invalid("S3 ARN buildspec references are not implemented; use inline YAML or a source-relative path")
		}

		if strings.Contains(common.Value(p.Source.Buildspec), "\n") {
			if _, parseErr := parseBuildspec([]byte(*p.Source.Buildspec)); parseErr != nil {
				return invalid(parseErr.Error())
			}
		}

		counterKey := meta.Key(serviceName, "counter", name)
		n, _, e := storage.Read[int64](tx, counterKey)
		if e != nil {
			return e
		}

		number := common.Value(n) + 1
		if e := storage.Write(tx, counterKey, number); e != nil {
			return e
		}

		hex := common.RandomID(32, "0123456789abcdef")
		uuid := hex[:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:]
		id := name + ":" + uuid
		initiator := meta.AccessKey
		if meta.Principal != nil {
			initiator = meta.Principal.Name
		}

		b := api.Build{
			Id:           new(id),
			Arn:          new(fmt.Sprintf("arn:aws:codebuild:%s:%s:build/%s", meta.Region, meta.AccountID, id)),
			ProjectName:  new(name),
			BuildNumber:  new(number),
			StartTime:    new(stamp()),
			CurrentPhase: new("QUEUED"),
			BuildStatus: new(
				"IN_PROGRESS",
			),
			BuildComplete:          new(false),
			Initiator:              new(initiator),
			SourceVersion:          p.SourceVersion,
			Source:                 p.Source,
			Environment:            p.Environment,
			ServiceRole:            p.ServiceRole,
			TimeoutInMinutes:       p.TimeoutInMinutes,
			QueuedTimeoutInMinutes: p.QueuedTimeoutInMinutes,
		}

		b.Phases = []api.BuildPhase{
			{
				PhaseType:         new("SUBMITTED"),
				PhaseStatus:       new("SUCCEEDED"),
				StartTime:         b.StartTime,
				EndTime:           b.StartTime,
				DurationInSeconds: new(int64(0)),
			},
			{PhaseType: new("QUEUED"), StartTime: b.StartTime},
		}

		group, stream := "/aws/codebuild/"+name, uuid
		enabled := true
		if p.LogsConfig != nil && p.LogsConfig.CloudWatchLogs != nil {
			lc := p.LogsConfig.CloudWatchLogs
			enabled = common.Value(lc.Status) != "DISABLED"
			group = common.Value(common.OrElse(lc.GroupName, new(group)))
			if lc.StreamName != nil {
				stream = *lc.StreamName + "/" + uuid
			}
		}

		if enabled {
			b.Logs = &api.LogsLocation{
				GroupName: new(group), StreamName: new(stream),
				DeepLink: new("https://console.aws.amazon.com/cloudwatch/home?region=" + meta.Region +
					"#logsV2:log-groups/log-group/" + group + "/log-events/" + stream),
			}
		}

		rec := buildRecord{Build: b, Project: *p, Scope: meta.Scope, Endpoint: meta.Endpoint}
		if e := storage.Write(tx, meta.Key(serviceName, buildKind, id), rec); e != nil {
			return e
		}

		if e := storage.Write(tx, indexKey(meta.Scope, id), meta.Key(serviceName, buildKind, id)); e != nil {
			return e
		}

		result = common.Clone(new(b))
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.emit(ctx, meta.Scope, result, false)
	return &api.StartBuildOutput{Build: result}, nil
}

func buildID(scope request.Scope, id string) string {
	prefix := fmt.Sprintf("arn:aws:codebuild:%s:%s:build/", scope.Region, scope.AccountID)
	return strings.TrimPrefix(id, prefix)
}

func (s *Service) BatchGetBuilds(ctx context.Context, in *api.BatchGetBuildsInput) (*api.BatchGetBuildsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.BatchGetBuildsOutput{Builds: []api.Build{}, BuildsNotFound: []string{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		seen := map[string]bool{}
		for _, id := range in.Ids {
			if seen[id] {
				continue
			}

			seen[id] = true
			r, ok, e := storage.Read[buildRecord](tx, scope.Key(serviceName, buildKind, buildID(scope, id)))
			if e != nil {
				return e
			}

			if ok {
				out.Builds = append(out.Builds, r.Build)
			} else {
				out.BuildsNotFound = append(out.BuildsNotFound, id)
			}
		}

		return nil
	})
	return out, err
}

func (s *Service) StopBuild(ctx context.Context, in *api.StopBuildInput) (*api.StopBuildOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id := buildID(scope, common.Value(in.Id))
	var b *api.Build
	changed := false
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, ok, e := storage.Read[buildRecord](tx, scope.Key(serviceName, buildKind, id))
		if e != nil {
			return e
		}

		if !ok {
			return missing()
		}

		if !common.Value(r.Build.BuildComplete) {
			complete(&r.Build, "STOPPED", "Build stopped by StopBuild")
			changed = true
		}

		b = common.Clone(new(r.Build))
		return storage.Write(tx, scope.Key(serviceName, buildKind, id), r)
	})
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	cancel := s.active[indexKey(scope, id)]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	if changed {
		event := common.Clone(b)
		event.Phases = event.Phases[:len(event.Phases)-1]
		s.emit(ctx, scope, event, true)
		s.emit(ctx, scope, b, false)
	}

	return &api.StopBuildOutput{Build: b}, nil
}

func (s *Service) listBuilds(
	ctx context.Context,
	project, order string,
	token *string,
) (ids []string, next *string, err error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	if order != "" && order != "ASCENDING" {
		return nil, nil, invalid("DESCENDING build ordering is not implemented")
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rows, e := storage.Records[buildRecord](tx, scope.Prefix(serviceName, buildKind))
		if e != nil {
			return e
		}

		for _, r := range rows {
			if project == "" || common.Value(r.Build.ProjectName) == project {
				ids = append(ids, common.Value(r.Build.Id))
			}
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return common.Page(ids, func(id string) string { return id }, token, 100, invalid("Invalid nextToken"))
}

func (s *Service) ListBuilds(ctx context.Context, in *api.ListBuildsInput) (*api.ListBuildsOutput, error) {
	ids, next, err := s.listBuilds(ctx, "", common.Value(in.SortOrder), in.NextToken)
	return &api.ListBuildsOutput{Ids: ids, NextToken: next}, err
}

func (s *Service) ListBuildsForProject(
	ctx context.Context,
	in *api.ListBuildsForProjectInput,
) (*api.ListBuildsForProjectOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, local, err := projectIdentifier(scope, common.Value(in.ProjectName))
	if err != nil {
		return nil, err
	}

	if !local {
		return nil, missing()
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, ok, e := read(tx, scope, name)
		if e != nil {
			return e
		}

		if !ok {
			return missing()
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	ids, next, err := s.listBuilds(ctx, name, common.Value(in.SortOrder), in.NextToken)
	return &api.ListBuildsForProjectOutput{Ids: ids, NextToken: next}, err
}

func (s *Service) BatchDeleteBuilds(
	ctx context.Context,
	in *api.BatchDeleteBuildsInput,
) (*api.BatchDeleteBuildsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.BatchDeleteBuildsOutput{BuildsDeleted: []string{}, BuildsNotDeleted: []api.BuildNotDeleted{}}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, input := range in.Ids {
			id := buildID(scope, input)
			key := scope.Key(serviceName, buildKind, id)
			r, ok, e := storage.Read[buildRecord](tx, key)
			if e != nil {
				return e
			}

			if !ok || !common.Value(r.Build.BuildComplete) {
				status := "BUILD_NOT_FOUND"
				if ok {
					status = "BUILD_IN_PROGRESS"
				}

				out.BuildsNotDeleted = append(
					out.BuildsNotDeleted,
					api.BuildNotDeleted{Id: new(input), StatusCode: new(status)},
				)
				continue
			}

			tx.Delete(key)
			tx.Delete(indexKey(scope, id))
			out.BuildsDeleted = append(out.BuildsDeleted, input)
		}

		return nil
	})
	return out, err
}

// pipelineSourceKey is private so only StartPipelineBuild can supply a pipeline artifact.
type pipelineSourceKey struct{}

// StartPipelineBuild runs a CodePipeline artifact through the existing S3 build runner.
func (s *Service) StartPipelineBuild(
	ctx context.Context,
	in *api.StartBuildInput,
	source string,
) (*api.StartBuildOutput, error) {
	return s.StartBuild(context.WithValue(ctx, pipelineSourceKey{}, source), in)
}
