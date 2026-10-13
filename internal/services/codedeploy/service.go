// Package codedeploy stores applications, deployment configurations and groups.
// No deployments, agents, lifecycle hooks or traffic changes are executed.
package codedeploy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codedeploy"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	allAtOnce       = "AllAtOnce"
	timeBasedCanary = "TimeBasedCanary"
	timeBasedLinear = "TimeBasedLinear"
	fleetPercent    = "FLEET_PERCENT"
	serviceName     = "codedeploy"
	applicationKind = "application"
	configKind      = "deployment-config"
	groupKind       = "deployment-group"
	server          = "Server"
	lambda          = "Lambda"
	ecs             = "ECS"
	oneAtATime      = "CodeDeployDefault.OneAtATime"
)

var _ api.API = (*Service)(nil)

type (
	Service struct{ store storage.Store }
	record  struct {
		Application *api.ApplicationInfo
		Config      *api.DeploymentConfigInfo
		Group       *api.DeploymentGroupInfo
		Tags        map[string]string
	}
)

func New(store storage.Store) *Service { return &Service{store: store} }

func resourceARN(scope request.Scope, kind, name string) string {
	return fmt.Sprintf("arn:aws:codedeploy:%s:%s:%s:%s", scope.Region, scope.AccountID, kind, name)
}

func readApplication(tx storage.ReadTx, scope request.Scope, name *string) (*record, error) {
	if validationErr := checkName(name, "Application"); validationErr != nil {
		return nil, validationErr
	}

	r, ok, err := storage.Read[record](tx, scope.Key(serviceName, applicationKind, *name))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, awserror.New("ApplicationDoesNotExistException", "Application does not exist")
	}

	return r, nil
}

func readGroup(tx storage.ReadTx, scope request.Scope, app, name *string) (*record, error) {
	if _, err := readApplication(tx, scope, app); err != nil {
		return nil, err
	}

	if validationErr := checkName(name, "DeploymentGroup"); validationErr != nil {
		return nil, validationErr
	}

	r, ok, err := storage.Read[record](tx, scope.Key(serviceName, groupKind, *app+"/"+*name))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, awserror.New("DeploymentGroupDoesNotExistException", "Deployment group does not exist")
	}

	return r, nil
}

func (s *Service) CreateApplication(
	ctx context.Context,
	in *api.CreateApplicationInput,
) (*api.CreateApplicationOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, awserror.New("ApplicationNameRequiredException", "applicationName is required")
	}

	in = common.Clone(in)
	if validationErr := checkName(in.ApplicationName, "Application"); validationErr != nil {
		return nil, validationErr
	}

	platform := common.OrElse(in.ComputePlatform, new(server))
	if validationErr := checkPlatform(platform); validationErr != nil {
		return nil, validationErr
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	r := &record{Application: &api.ApplicationInfo{
		ApplicationName: in.ApplicationName, ApplicationId: new(common.RandomID(32)),
		ComputePlatform: platform, CreateTime: common.SecondsMilli(time.Now()), LinkedToGitHub: new(false),
	}, Tags: tags}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, applicationKind, *in.ApplicationName)
		if _, ok := tx.Get(key); ok {
			return awserror.New("ApplicationAlreadyExistsException", "Application already exists")
		}

		return storage.Write(tx, key, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateApplicationOutput{ApplicationId: r.Application.ApplicationId}, nil
}

func (s *Service) GetApplication(ctx context.Context, in *api.GetApplicationInput) (*api.GetApplicationOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		in = &api.GetApplicationInput{}
	}

	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = readApplication(tx, scope, in.ApplicationName)
		return e
	})
	if err != nil {
		return nil, err
	}

	return &api.GetApplicationOutput{Application: r.Application}, nil
}

func (s *Service) BatchGetApplications(
	ctx context.Context,
	in *api.BatchGetApplicationsInput,
) (*api.BatchGetApplicationsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		in = &api.BatchGetApplicationsInput{}
	}

	if validationErr := checkBatch(in.ApplicationNames, "Application"); validationErr != nil {
		return nil, validationErr
	}

	out := &api.BatchGetApplicationsOutput{ApplicationsInfo: []api.ApplicationInfo{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, name := range in.ApplicationNames {
			r, e := readApplication(tx, scope, new(name))
			if e != nil {
				return e
			}

			out.ApplicationsInfo = append(out.ApplicationsInfo, *r.Application)
		}

		return nil
	})
	return out, err
}

func (s *Service) ListApplications(
	ctx context.Context,
	in *api.ListApplicationsInput,
) (*api.ListApplicationsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		in = &api.ListApplicationsInput{}
	}

	var names []string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		prefix := scope.Prefix(serviceName, applicationKind)
		for _, key := range tx.Keys(prefix) {
			names = append(names, strings.TrimPrefix(key, prefix))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(names, func(n string) string { return n }, in.NextToken, 100,
		awserror.New("InvalidNextTokenException", "Invalid nextToken"))
	return &api.ListApplicationsOutput{Applications: page, NextToken: next}, err
}

func (s *Service) UpdateApplication(
	ctx context.Context,
	in *api.UpdateApplicationInput,
) (*api.UpdateApplicationOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		in = &api.UpdateApplicationInput{}
	}

	if in.NewApplicationName != nil {
		if validationErr := checkName(in.NewApplicationName, "Application"); validationErr != nil {
			return nil, validationErr
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readApplication(tx, scope, in.ApplicationName)
		if e != nil {
			return e
		}

		if in.NewApplicationName == nil || *in.NewApplicationName == *in.ApplicationName {
			return nil
		}

		key := scope.Key(serviceName, applicationKind, *in.NewApplicationName)
		if _, ok := tx.Get(key); ok {
			return awserror.New("ApplicationAlreadyExistsException", "Application already exists")
		}

		r.Application.ApplicationName = new(*in.NewApplicationName)
		prefix := scope.Key(serviceName, groupKind, *in.ApplicationName) + "/"
		for _, old := range tx.Keys(prefix) {
			g, _, e := storage.Read[record](tx, old)
			if e != nil {
				return e
			}

			g.Group.ApplicationName = new(*in.NewApplicationName)
			if e := storage.Write(
				tx,
				scope.Key(serviceName, groupKind, *in.NewApplicationName+"/"+*g.Group.DeploymentGroupName),
				g,
			); e != nil {
				return e
			}

			tx.Delete(old)
		}

		tx.Delete(scope.Key(serviceName, applicationKind, *in.ApplicationName))
		return storage.Write(tx, key, r)
	})
	return &api.UpdateApplicationOutput{}, err
}

func (s *Service) DeleteApplication(
	ctx context.Context,
	in *api.DeleteApplicationInput,
) (*api.DeleteApplicationOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		in = &api.DeleteApplicationInput{}
	}

	if validationErr := checkName(in.ApplicationName, "Application"); validationErr != nil {
		return nil, validationErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, key := range tx.Keys(scope.Key(serviceName, groupKind, *in.ApplicationName) + "/") {
			tx.Delete(key)
		}

		tx.Delete(scope.Key(serviceName, applicationKind, *in.ApplicationName))
		return nil
	})
	return &api.DeleteApplicationOutput{}, err
}
