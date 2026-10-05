package apigateway

import (
	"context"
	"regexp"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// deployment is a deployment's description and the snapshot of the API that
// stages serving it run.
type deployment struct {
	Resources   map[string]*api.Resource
	Authorizers map[string]*api.Authorizer
	Deployment  api.Deployment
}

var (
	stageName     = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
	stageVariable = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
	logGroupARN   = regexp.MustCompile(`^arn:aws[a-z-]*:logs:[a-z0-9-]+:\d{12}:log-group:[\w\-./#]{1,512}(:\*)?$`)
)

// snapshot captures the API for a deployment, which needs every method to
// have an integration.
func snapshot(a *restAPI) (*deployment, error) {
	methodCount := 0
	for _, r := range a.Resources {
		for name, m := range r.ResourceMethods {
			if m.MethodIntegration == nil {
				return nil, badRequest("No integration defined for method %s %s", name, common.Value(r.Path))
			}

			methodCount++
		}
	}

	if methodCount == 0 {
		return nil, badRequest("The REST API doesn't contain any methods")
	}

	c := common.Clone(a)
	return &deployment{Resources: c.Resources, Authorizers: c.Authorizers}, nil
}

func checkStage(tx storage.ReadTx, scope request.Scope, apiID string, st *api.Stage) error {
	if _, found := tx.Get(owned(scope, kindDeployment, apiID, common.Value(st.DeploymentId))); !found {
		return badRequest("Invalid deployment identifier specified")
	}

	for name, v := range st.Variables {
		if !stageVariable.MatchString(name) || len(v) > 512 {
			return badRequest("Invalid stage variable %s.", name)
		}
	}

	if logs := st.AccessLogSettings; logs != nil && (logs.DestinationArn != nil || logs.Format != nil) {
		if !logGroupARN.MatchString(common.Value(logs.DestinationArn)) || common.Value(logs.Format) == "" {
			return badRequest("Access log settings need a CloudWatch Logs log group ARN and a format.")
		}
	}

	return nil
}

func newStage(name, deploymentID, description *string, variables map[string]string, now float64) *api.Stage {
	return &api.Stage{
		StageName: name, DeploymentId: deploymentID, Description: description, Variables: variables,
		CacheClusterEnabled: new(false), CacheClusterStatus: new("NOT_AVAILABLE"), TracingEnabled: new(false),
		MethodSettings: map[string]api.MethodSetting{}, Tags: map[string]string{}, CreatedDate: &now,
		LastUpdatedDate: &now,
	}
}

func (s *Service) CreateDeployment(ctx context.Context, in *api.CreateDeploymentRequest) (*api.Deployment, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	// An empty stage name, as Terraform sends, deploys to no stage.
	if common.Value(in.StageName) == "" {
		in.StageName = nil
	}

	if in.StageName != nil && !stageName.MatchString(*in.StageName) {
		return nil, badRequest("Stage name only allows a-zA-Z0-9_- and must be 1 to 128 characters.")
	}

	var d *deployment
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.RestApiId)
		if loadErr != nil {
			return loadErr
		}

		if d, loadErr = snapshot(a); loadErr != nil {
			return loadErr
		}

		now := common.Seconds(s.now())
		d.Deployment = api.Deployment{Id: new(common.RandomID(6)), Description: in.Description, CreatedDate: now}
		apiID := *a.API.Id
		if writeErr := storage.Write(tx, owned(m.Scope, kindDeployment, apiID, *d.Deployment.Id), d); writeErr != nil {
			return writeErr
		}

		if in.StageName == nil {
			return nil
		}

		key := owned(m.Scope, kindStage, apiID, *in.StageName)
		st, found, readErr := storage.Read[api.Stage](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			st = newStage(in.StageName, d.Deployment.Id, in.StageDescription, in.Variables, *now)
		} else {
			st.DeploymentId, st.LastUpdatedDate = d.Deployment.Id, now
			if in.Variables != nil {
				st.Variables = in.Variables
			}
		}

		if checkErr := checkStage(tx, m.Scope, apiID, st); checkErr != nil {
			return checkErr
		}

		return storage.Write(tx, key, st)
	})
	if err != nil {
		return nil, err
	}

	return &d.Deployment, nil
}

func loadDeployment(tx storage.ReadTx, scope request.Scope, apiID string, id *string) (*deployment, error) {
	d, found, err := storage.Read[deployment](tx, owned(scope, kindDeployment, apiID, common.Value(id)))
	if err != nil || !found {
		return nil, common.ErrorsOr(err, notFound("Invalid Deployment identifier specified"))
	}

	return d, nil
}

func (s *Service) GetDeployment(ctx context.Context, in *api.GetDeploymentRequest) (*api.Deployment, error) {
	var out *api.Deployment
	err := s.viewAPI(ctx, in.RestApiId, func(tx storage.ReadTx, scope request.Scope, a *restAPI) error {
		d, err := loadDeployment(tx, scope, *a.API.Id, in.DeploymentId)
		if err == nil {
			out = &d.Deployment
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetDeployments(ctx context.Context, in *api.GetDeploymentsRequest) (*api.Deployments, error) {
	out := &api.Deployments{Items: []api.Deployment{}}
	err := s.viewAPI(ctx, in.RestApiId, func(tx storage.ReadTx, scope request.Scope, a *restAPI) error {
		all, err := storage.Records[deployment](tx, prefix(scope, kindDeployment)+*a.API.Id+"/")
		if err != nil {
			return err
		}

		items := make([]api.Deployment, 0, len(all))
		for _, d := range all {
			items = append(items, d.Deployment)
		}

		items, out.Position, err = page(items, func(d api.Deployment) string { return *d.Id }, in.Position, in.Limit)
		out.Items = append(out.Items, items...)
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateDeployment(ctx context.Context, in *api.UpdateDeploymentRequest) (*api.Deployment, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var d *deployment
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.RestApiId)
		if loadErr != nil {
			return loadErr
		}

		if d, loadErr = loadDeployment(tx, m.Scope, *a.API.Id, in.DeploymentId); loadErr != nil {
			return loadErr
		}

		if patchErr := patch(&d.Deployment, in.PatchOperations, "description"); patchErr != nil {
			return patchErr
		}

		return storage.Write(tx, owned(m.Scope, kindDeployment, *a.API.Id, *d.Deployment.Id), d)
	})
	if err != nil {
		return nil, err
	}

	return &d.Deployment, nil
}

func (s *Service) DeleteDeployment(
	ctx context.Context,
	in *api.DeleteDeploymentRequest,
) (*api.DeleteDeploymentOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.RestApiId)
		if loadErr != nil {
			return loadErr
		}

		d, loadErr := loadDeployment(tx, m.Scope, *a.API.Id, in.DeploymentId)
		if loadErr != nil {
			return loadErr
		}

		stages, readErr := storage.Records[api.Stage](tx, prefix(m.Scope, kindStage)+*a.API.Id+"/")
		if readErr != nil {
			return readErr
		}

		for _, st := range stages {
			if common.Value(st.DeploymentId) == *d.Deployment.Id {
				return badRequest("Active stages pointing to this deployment must be moved or deleted")
			}
		}

		tx.Delete(owned(m.Scope, kindDeployment, *a.API.Id, *d.Deployment.Id))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteDeploymentOutput{}, nil
}

func (s *Service) CreateStage(ctx context.Context, in *api.CreateStageRequest) (*api.Stage, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if !stageName.MatchString(common.Value(in.StageName)) {
		return nil, badRequest("Stage name only allows a-zA-Z0-9_- and must be 1 to 128 characters.")
	}

	if checkErr := checkTags(in.Tags); checkErr != nil {
		return nil, checkErr
	}

	st := newStage(in.StageName, in.DeploymentId, in.Description, in.Variables, *common.Seconds(s.now()))
	st.TracingEnabled, st.Tags = new(common.Value(in.TracingEnabled)), common.CloneTags(in.Tags)
	st.CacheClusterEnabled, st.CacheClusterSize = new(common.Value(in.CacheClusterEnabled)), in.CacheClusterSize
	if common.Value(st.CacheClusterEnabled) {
		st.CacheClusterStatus = new("AVAILABLE")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.RestApiId)
		if loadErr != nil {
			return loadErr
		}

		key := owned(m.Scope, kindStage, *a.API.Id, *st.StageName)
		if _, found := tx.Get(key); found {
			return conflict("Stage already exists")
		}

		if checkErr := checkStage(tx, m.Scope, *a.API.Id, st); checkErr != nil {
			return checkErr
		}

		return storage.Write(tx, key, st)
	})
	if err != nil {
		return nil, err
	}

	return st, nil
}

func loadStage(tx storage.ReadTx, scope request.Scope, apiID string, name *string) (*api.Stage, error) {
	st, found, err := storage.Read[api.Stage](tx, owned(scope, kindStage, apiID, common.Value(name)))
	if err != nil || !found {
		return nil, common.ErrorsOr(err, notFound("Invalid stage identifier specified"))
	}

	return st, nil
}

func (s *Service) GetStage(ctx context.Context, in *api.GetStageRequest) (*api.Stage, error) {
	var out *api.Stage
	err := s.viewAPI(ctx, in.RestApiId, func(tx storage.ReadTx, scope request.Scope, a *restAPI) error {
		var err error
		out, err = loadStage(tx, scope, *a.API.Id, in.StageName)
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetStages(ctx context.Context, in *api.GetStagesRequest) (*api.Stages, error) {
	out := &api.Stages{Item: []api.Stage{}}
	err := s.viewAPI(ctx, in.RestApiId, func(tx storage.ReadTx, scope request.Scope, a *restAPI) error {
		all, err := storage.Records[api.Stage](tx, prefix(scope, kindStage)+*a.API.Id+"/")
		for _, st := range all {
			if in.DeploymentId == nil || common.Value(st.DeploymentId) == *in.DeploymentId {
				out.Item = append(out.Item, *st)
			}
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// methodSettingFields maps a stage method setting's patch path to its member.
var methodSettingFields = map[string]string{
	"metrics/enabled":                                "metricsEnabled",
	"logging/loglevel":                               "loggingLevel",
	"logging/dataTrace":                              "dataTraceEnabled",
	"throttling/burstLimit":                          "throttlingBurstLimit",
	"throttling/rateLimit":                           "throttlingRateLimit",
	"caching/enabled":                                "cachingEnabled",
	"caching/ttlInSeconds":                           "cacheTtlInSeconds",
	"caching/dataEncrypted":                          "cacheDataEncrypted",
	"caching/requireAuthorizationForCacheControl":    "requireAuthorizationForCacheControl",
	"caching/unauthorizedCacheControlHeaderStrategy": "unauthorizedCacheControlHeaderStrategy",
}

var stageFields = []string{
	"description", "deploymentId", "variables", "tracingEnabled", "cacheClusterEnabled", "cacheClusterSize",
	"accessLogSettings",
}

// patchStage applies a stage's patch operations: its own members, and method
// settings at /<resource path>/<method>/<category>/<setting> (such as
// /*/*/metrics/enabled), whose settings are stored under
// "<resource path>/<method>".
func patchStage(st *api.Stage, ops []api.PatchOperation) error {
	for _, op := range ops {
		segments := pathSegments(common.Value(op.Path))
		if len(segments) > 0 && slices.Contains(stageFields, segments[0]) {
			if err := patch(st, []api.PatchOperation{op}, stageFields...); err != nil {
				return err
			}

			continue
		}

		n := len(segments)
		if n < 2 {
			return badRequest("Invalid patch path %s", common.Value(op.Path))
		}

		setting, known := "", false
		if n >= 4 {
			setting, known = methodSettingFields[segments[n-2]+"/"+segments[n-1]]
		}

		methodPath := strings.TrimPrefix(strings.Join(segments[:n-2], "/"), "/")
		if !known {
			methodPath = strings.TrimPrefix(strings.Join(segments, "/"), "/")
		}

		if common.Value(op.Op) == opRemove && !known {
			delete(st.MethodSettings, methodPath)
			continue
		}

		if !known {
			return badRequest("Invalid method setting path: %s", common.Value(op.Path))
		}

		ms := st.MethodSettings[methodPath]
		op.Path = new("/" + setting)
		if err := patch(&ms, []api.PatchOperation{op}, setting); err != nil {
			return err
		}

		if st.MethodSettings == nil {
			st.MethodSettings = map[string]api.MethodSetting{}
		}

		st.MethodSettings[methodPath] = ms
	}

	return nil
}

func (s *Service) UpdateStage(ctx context.Context, in *api.UpdateStageRequest) (*api.Stage, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var st *api.Stage
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.RestApiId)
		if loadErr != nil {
			return loadErr
		}

		if st, loadErr = loadStage(tx, m.Scope, *a.API.Id, in.StageName); loadErr != nil {
			return loadErr
		}

		if patchErr := patchStage(st, in.PatchOperations); patchErr != nil {
			return patchErr
		}

		st.CacheClusterStatus = new("NOT_AVAILABLE")
		if common.Value(st.CacheClusterEnabled) {
			st.CacheClusterStatus = new("AVAILABLE")
		}

		if st.AccessLogSettings != nil && st.AccessLogSettings.DestinationArn == nil &&
			st.AccessLogSettings.Format == nil {
			st.AccessLogSettings = nil
		}

		st.LastUpdatedDate = common.Seconds(s.now())
		if checkErr := checkStage(tx, m.Scope, *a.API.Id, st); checkErr != nil {
			return checkErr
		}

		return storage.Write(tx, owned(m.Scope, kindStage, *a.API.Id, *st.StageName), st)
	})
	if err != nil {
		return nil, err
	}

	return st, nil
}

func (s *Service) DeleteStage(ctx context.Context, in *api.DeleteStageRequest) (*api.DeleteStageOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.RestApiId)
		if loadErr != nil {
			return loadErr
		}

		st, loadErr := loadStage(tx, m.Scope, *a.API.Id, in.StageName)
		if loadErr != nil {
			return loadErr
		}

		tx.Delete(owned(m.Scope, kindStage, *a.API.Id, *st.StageName))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteStageOutput{}, nil
}
