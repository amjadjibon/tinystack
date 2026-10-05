package apigatewayv2

import (
	"context"
	"regexp"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const deployed = "DEPLOYED"

// deployment is a deployment's description and the snapshot of the API that
// stages serving it run.
type deployment struct {
	Integrations map[string]api.Integration
	Authorizers  map[string]api.Authorizer
	Deployment   api.Deployment
	// Stage is the stage an automatic deployment was made for.
	Stage  string `json:",omitempty"`
	Routes []api.Route
}

// snapshot captures the API's routes, integrations, and authorizers.
func snapshot(tx storage.ReadTx, scope request.Scope, apiID string, now time.Time) (*deployment, error) {
	d := &deployment{
		Deployment: api.Deployment{
			DeploymentId: new(
				common.RandomID(6),
			),
			CreatedDate:      &now,
			DeploymentStatus: new(deployed),
			AutoDeployed:     new(false),
		},
		Integrations: map[string]api.Integration{}, Authorizers: map[string]api.Authorizer{},
	}
	routes, err := storage.Records[api.Route](tx, prefix(scope, kindRoute)+apiID+"/")
	if err != nil {
		return nil, err
	}

	for _, r := range routes {
		d.Routes = append(d.Routes, *r)
	}

	integrations, err := storage.Records[api.Integration](tx, prefix(scope, kindIntegration)+apiID+"/")
	if err != nil {
		return nil, err
	}

	for _, i := range integrations {
		d.Integrations[*i.IntegrationId] = *i
	}

	authorizers, err := storage.Records[api.Authorizer](tx, prefix(scope, kindAuthorizer)+apiID+"/")
	if err != nil {
		return nil, err
	}

	for _, a := range authorizers {
		d.Authorizers[*a.AuthorizerId] = *a
	}

	return d, nil
}

func deployedMessage(id string) *string {
	return new("Successfully deployed stage with deployment ID '" + id + "'")
}

// deploy deploys the API to every stage that deploys automatically, deleting
// each stage's previous automatic deployment unless another stage serves it.
func (s *Service) deploy(tx storage.WriteTx, scope request.Scope, apiID string, now time.Time) error {
	stages, err := storage.Records[api.Stage](tx, prefix(scope, kindStage)+apiID+"/")
	if err != nil {
		return err
	}

	for _, st := range stages {
		if common.Value(st.AutoDeploy) {
			if deployErr := s.deployStage(tx, scope, apiID, st, now); deployErr != nil {
				return deployErr
			}
		}
	}

	return nil
}

// deployStage makes a new automatic deployment for st and stores st.
func (s *Service) deployStage(
	tx storage.WriteTx,
	scope request.Scope,
	apiID string,
	st *api.Stage,
	now time.Time,
) error {
	d, err := snapshot(tx, scope, apiID, now)
	if err != nil {
		return err
	}

	d.Deployment.AutoDeployed, d.Stage = new(true), *st.StageName
	d.Deployment.Description = new("Automatic deployment triggered by changes to the Api configuration")
	if writeErr := storage.Write(
		tx,
		owned(scope, kindDeployment, apiID, *d.Deployment.DeploymentId),
		d,
	); writeErr != nil {
		return writeErr
	}

	previous := common.Value(st.DeploymentId)
	st.DeploymentId, st.LastUpdatedDate = d.Deployment.DeploymentId, &now
	st.LastDeploymentStatusMessage = deployedMessage(*d.Deployment.DeploymentId)
	if writeErr := storage.Write(tx, owned(scope, kindStage, apiID, *st.StageName), st); writeErr != nil {
		return writeErr
	}

	return prune(tx, scope, apiID, previous)
}

// prune deletes an automatic deployment that no stage serves.
func prune(tx storage.WriteTx, scope request.Scope, apiID, id string) error {
	key := owned(scope, kindDeployment, apiID, id)
	d, found, err := storage.Read[deployment](tx, key)
	if err != nil || !found || !common.Value(d.Deployment.AutoDeployed) {
		return err
	}

	stages, err := storage.Records[api.Stage](tx, prefix(scope, kindStage)+apiID+"/")
	if err != nil {
		return err
	}

	for _, st := range stages {
		if common.Value(st.DeploymentId) == id {
			return nil
		}
	}

	tx.Delete(key)
	return nil
}

var (
	stageName = regexp.MustCompile(`^[a-zA-Z0-9\-._~:/?#&=,]{1,128}$`)
	logGroup  = regexp.MustCompile(`^arn:aws[a-z-]*:logs:[a-z0-9-]+:\d{12}:log-group:[\w\-./#]{1,512}(:\*)?$`)
)

// checkStage validates a stage's access log settings and deployment.
func checkStage(tx storage.ReadTx, scope request.Scope, apiID string, st *api.Stage) error {
	if logs := st.AccessLogSettings; logs != nil && (logs.DestinationArn != nil || logs.Format != nil) {
		if !logGroup.MatchString(common.Value(logs.DestinationArn)) || common.Value(logs.Format) == "" {
			return badRequest("AccessLogSettings need a CloudWatch Logs log group ARN and a format.")
		}
	}

	for name, v := range st.StageVariables {
		if !stageVariable.MatchString(name) || len(v) > 2048 {
			return badRequest("Invalid stage variable %s.", name)
		}
	}

	if id := common.Value(st.DeploymentId); id != "" {
		if _, found := tx.Get(owned(scope, kindDeployment, apiID, id)); !found {
			return badRequest("Deployment %s does not exist.", id)
		}
	}

	return nil
}

var stageVariable = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

func (s *Service) CreateStage(ctx context.Context, in *api.CreateStageRequest) (*api.CreateStageResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.StageName)
	if name != defaultStage && !stageName.MatchString(name) {
		return nil, badRequest("Invalid stage name %s.", name)
	}

	if checkErr := checkTags(in.Tags); checkErr != nil {
		return nil, checkErr
	}

	now := s.now().UTC()
	st := &api.Stage{
		StageName: in.StageName, Description: in.Description, AutoDeploy: new(common.Value(in.AutoDeploy)),
		DeploymentId: in.DeploymentId, AccessLogSettings: in.AccessLogSettings, CreatedDate: &now,
		LastUpdatedDate: &now, DefaultRouteSettings: in.DefaultRouteSettings, RouteSettings: in.RouteSettings,
		StageVariables: in.StageVariables, Tags: common.CloneTags(in.Tags),
	}
	if st.DefaultRouteSettings == nil {
		st.DefaultRouteSettings = &api.RouteSettings{DetailedMetricsEnabled: new(false)}
	}

	if st.RouteSettings == nil {
		st.RouteSettings = map[string]api.RouteSettings{}
	}

	if st.StageVariables == nil {
		st.StageVariables = map[string]string{}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		key := owned(m.Scope, kindStage, *a.ApiId, name)
		if _, found := tx.Get(key); found {
			return conflict("Stage %s already exists", name)
		}

		if checkErr := checkStage(tx, m.Scope, *a.ApiId, st); checkErr != nil {
			return checkErr
		}

		if common.Value(st.AutoDeploy) {
			return s.deployStage(tx, m.Scope, *a.ApiId, st, now)
		}

		if st.DeploymentId != nil {
			st.LastDeploymentStatusMessage = deployedMessage(*st.DeploymentId)
		}

		return storage.Write(tx, key, st)
	})
	if err != nil {
		return nil, err
	}

	out := api.CreateStageResponse(*st)
	return &out, nil
}

func loadStage(tx storage.ReadTx, scope request.Scope, apiID string, name *string) (*api.Stage, error) {
	st, found, err := storage.Read[api.Stage](tx, owned(scope, kindStage, apiID, common.Value(name)))
	if err != nil || !found {
		return nil, common.ErrorsOr(err, notFound("Invalid stage identifier specified"))
	}

	return st, nil
}

func (s *Service) GetStage(ctx context.Context, in *api.GetStageRequest) (*api.GetStageResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var st *api.Stage
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		st, loadErr = loadStage(tx, m.Scope, *a.ApiId, in.StageName)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	out := api.GetStageResponse(*st)
	return &out, nil
}

func (s *Service) GetStages(ctx context.Context, in *api.GetStagesRequest) (*api.GetStagesResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.GetStagesResponse{Items: []api.Stage{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		all, readErr := storage.Records[api.Stage](tx, prefix(m.Scope, kindStage)+*a.ApiId+"/")
		if readErr != nil {
			return readErr
		}

		items := make([]api.Stage, 0, len(all))
		for _, st := range all {
			items = append(items, *st)
		}

		items, out.NextToken, readErr = page(items, func(st api.Stage) string { return *st.StageName },
			in.NextToken, in.MaxResults)
		out.Items = append(out.Items, items...)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateStage(ctx context.Context, in *api.UpdateStageRequest) (*api.UpdateStageResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var st *api.Stage
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		if st, loadErr = loadStage(tx, m.Scope, *a.ApiId, in.StageName); loadErr != nil {
			return loadErr
		}

		wasAuto, previous := common.Value(st.AutoDeploy), common.Value(st.DeploymentId)
		now := s.now().UTC()
		st.Description, st.AutoDeploy = common.OrElse(
			in.Description,
			st.Description,
		), common.OrElse(
			in.AutoDeploy,
			st.AutoDeploy,
		)
		st.DeploymentId = common.OrElse(in.DeploymentId, st.DeploymentId)
		st.AccessLogSettings = common.OrElse(in.AccessLogSettings, st.AccessLogSettings)
		st.DefaultRouteSettings = common.OrElse(in.DefaultRouteSettings, st.DefaultRouteSettings)
		st.LastUpdatedDate = &now
		if in.RouteSettings != nil {
			st.RouteSettings = in.RouteSettings
		}

		if in.StageVariables != nil {
			st.StageVariables = in.StageVariables
		}

		if checkErr := checkStage(tx, m.Scope, *a.ApiId, st); checkErr != nil {
			return checkErr
		}

		if common.Value(st.AutoDeploy) && !wasAuto {
			return s.deployStage(tx, m.Scope, *a.ApiId, st, now)
		}

		if common.Value(st.DeploymentId) != previous {
			st.LastDeploymentStatusMessage = deployedMessage(common.Value(st.DeploymentId))
		}

		if writeErr := storage.Write(tx, owned(m.Scope, kindStage, *a.ApiId, *st.StageName), st); writeErr != nil {
			return writeErr
		}

		return prune(tx, m.Scope, *a.ApiId, previous)
	})
	if err != nil {
		return nil, err
	}

	out := api.UpdateStageResponse(*st)
	return &out, nil
}

func (s *Service) DeleteStage(ctx context.Context, in *api.DeleteStageRequest) (*api.DeleteStageOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		st, loadErr := loadStage(tx, m.Scope, *a.ApiId, in.StageName)
		if loadErr != nil {
			return loadErr
		}

		tx.Delete(owned(m.Scope, kindStage, *a.ApiId, *st.StageName))
		return prune(tx, m.Scope, *a.ApiId, common.Value(st.DeploymentId))
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteStageOutput{}, nil
}

func (s *Service) CreateDeployment(
	ctx context.Context,
	in *api.CreateDeploymentRequest,
) (*api.CreateDeploymentResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var d *deployment
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		now := s.now().UTC()
		if d, loadErr = snapshot(tx, m.Scope, *a.ApiId, now); loadErr != nil {
			return loadErr
		}

		d.Deployment.Description = in.Description
		if in.StageName != nil {
			st, stageErr := loadStage(tx, m.Scope, *a.ApiId, in.StageName)
			if stageErr != nil {
				return stageErr
			}

			if common.Value(st.AutoDeploy) {
				return badRequest("Stage %s deploys automatically; turn AutoDeploy off to deploy it yourself.",
					*st.StageName)
			}

			previous := common.Value(st.DeploymentId)
			st.DeploymentId, st.LastUpdatedDate = d.Deployment.DeploymentId, &now
			st.LastDeploymentStatusMessage = deployedMessage(*d.Deployment.DeploymentId)
			if writeErr := storage.Write(tx, owned(m.Scope, kindStage, *a.ApiId, *st.StageName), st); writeErr != nil {
				return writeErr
			}

			if pruneErr := prune(tx, m.Scope, *a.ApiId, previous); pruneErr != nil {
				return pruneErr
			}
		}

		return storage.Write(tx, owned(m.Scope, kindDeployment, *a.ApiId, *d.Deployment.DeploymentId), d)
	})
	if err != nil {
		return nil, err
	}

	out := api.CreateDeploymentResponse(d.Deployment)
	return &out, nil
}

func loadDeployment(tx storage.ReadTx, scope request.Scope, apiID string, id *string) (*deployment, error) {
	d, found, err := storage.Read[deployment](tx, owned(scope, kindDeployment, apiID, common.Value(id)))
	if err != nil || !found {
		return nil, common.ErrorsOr(err, notFound("Invalid Deployment identifier specified %s", common.Value(id)))
	}

	return d, nil
}

func (s *Service) GetDeployment(
	ctx context.Context,
	in *api.GetDeploymentRequest,
) (*api.GetDeploymentResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var d *deployment
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		d, loadErr = loadDeployment(tx, m.Scope, *a.ApiId, in.DeploymentId)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	out := api.GetDeploymentResponse(d.Deployment)
	return &out, nil
}

func (s *Service) GetDeployments(
	ctx context.Context,
	in *api.GetDeploymentsRequest,
) (*api.GetDeploymentsResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.GetDeploymentsResponse{Items: []api.Deployment{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		all, readErr := storage.Records[deployment](tx, prefix(m.Scope, kindDeployment)+*a.ApiId+"/")
		if readErr != nil {
			return readErr
		}

		items := make([]api.Deployment, 0, len(all))
		for _, d := range all {
			items = append(items, d.Deployment)
		}

		items, out.NextToken, readErr = page(items, func(d api.Deployment) string { return *d.DeploymentId },
			in.NextToken, in.MaxResults)
		out.Items = append(out.Items, items...)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateDeployment(
	ctx context.Context,
	in *api.UpdateDeploymentRequest,
) (*api.UpdateDeploymentResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var d *deployment
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		if d, loadErr = loadDeployment(tx, m.Scope, *a.ApiId, in.DeploymentId); loadErr != nil {
			return loadErr
		}

		d.Deployment.Description = common.OrElse(in.Description, d.Deployment.Description)
		return storage.Write(tx, owned(m.Scope, kindDeployment, *a.ApiId, *d.Deployment.DeploymentId), d)
	})
	if err != nil {
		return nil, err
	}

	out := api.UpdateDeploymentResponse(d.Deployment)
	return &out, nil
}

func (s *Service) DeleteDeployment(
	ctx context.Context,
	in *api.DeleteDeploymentRequest,
) (*api.DeleteDeploymentOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		d, loadErr := loadDeployment(tx, m.Scope, *a.ApiId, in.DeploymentId)
		if loadErr != nil {
			return loadErr
		}

		stages, readErr := storage.Records[api.Stage](tx, prefix(m.Scope, kindStage)+*a.ApiId+"/")
		if readErr != nil {
			return readErr
		}

		var active []string
		for _, st := range stages {
			if common.Value(st.DeploymentId) == *d.Deployment.DeploymentId {
				active = append(active, *st.StageName)
			}
		}

		if len(active) > 0 {
			return badRequest("Active stages pointing to this deployment must be moved or deleted: %s",
				strings.Join(active, ", "))
		}

		tx.Delete(owned(m.Scope, kindDeployment, *a.ApiId, *d.Deployment.DeploymentId))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteDeploymentOutput{}, nil
}
