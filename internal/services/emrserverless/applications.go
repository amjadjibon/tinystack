package emrserverless

import (
	"context"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/emrserverless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateApplication(
	ctx context.Context,
	in *api.CreateApplicationRequest,
) (*api.CreateApplicationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	tk, err := tokenKey(scope, "create", in.ClientToken)
	if err != nil {
		return nil, err
	}

	hash, err := requestHash(in)
	if err != nil {
		return nil, err
	}

	id := common.RandomID(16)
	a := api.Application{
		ApplicationId: new(
			id,
		),
		Arn:          new(applicationARN(scope, id)),
		Name:         in.Name,
		ReleaseLabel: in.ReleaseLabel,
		Type:         in.Type,
		Architecture: common.OrElse(
			in.Architecture,
			new("X86_64"),
		),
		InitialCapacity:        in.InitialCapacity,
		MaximumCapacity:        in.MaximumCapacity,
		AutoStartConfiguration: in.AutoStartConfiguration,
		AutoStopConfiguration:  in.AutoStopConfiguration,
		NetworkConfiguration:   in.NetworkConfiguration,
		Tags:                   common.CloneTags(in.Tags),
		State: new(
			stateCreated,
		),
		StateDetails: new(recordsOnly),
		CreatedAt:    common.SecondsMilli(time.Now()),
		UpdatedAt:    common.SecondsMilli(time.Now()),
	}

	if a.AutoStartConfiguration == nil {
		a.AutoStartConfiguration = &api.AutoStartConfig{Enabled: new(true)}
	}

	if a.AutoStopConfiguration == nil {
		a.AutoStopConfiguration = &api.AutoStopConfig{Enabled: new(true), IdleTimeoutMinutes: new(int64(15))}
	}

	if validationErr := checkApplication(&a); validationErr != nil {
		return nil, validationErr
	}

	if validationErr := checkTags(a.Tags); validationErr != nil {
		return nil, validationErr
	}

	out := &api.CreateApplicationResponse{ApplicationId: a.ApplicationId, Name: a.Name, Arn: a.Arn}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		previous, found, e := replay[api.CreateApplicationResponse](tx, tk, hash)
		if e != nil {
			return e
		}

		if found {
			out = &previous
			return nil
		}

		if e := storage.Write(tx, appKey(scope, id), &a); e != nil {
			return e
		}

		return storage.Write(tx, tk, tokenRecord[api.CreateApplicationResponse]{Hash: hash, Output: *out})
	})
	return out, err
}

func (s *Service) GetApplication(
	ctx context.Context,
	in *api.GetApplicationRequest,
) (*api.GetApplicationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var a *api.Application
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		a, e = readApplication(tx, scope, in.ApplicationId)
		return e
	})
	if err != nil {
		return nil, err
	}

	return &api.GetApplicationResponse{Application: a}, nil
}

func (s *Service) UpdateApplication(
	ctx context.Context,
	in *api.UpdateApplicationRequest,
) (*api.UpdateApplicationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	tk, err := tokenKey(scope, "update/"+common.Value(in.ApplicationId), in.ClientToken)
	if err != nil {
		return nil, err
	}

	hash, err := requestHash(in)
	if err != nil {
		return nil, err
	}

	var out *api.UpdateApplicationResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		previous, found, e := replay[api.UpdateApplicationResponse](tx, tk, hash)
		if e != nil {
			return e
		}

		if found {
			out = &previous
			return nil
		}

		a, e := readApplication(tx, scope, in.ApplicationId)
		if e != nil {
			return e
		}

		if !slices.Contains([]string{stateCreated, stateStopped}, common.Value(a.State)) {
			return conflict("Stop the application before updating it.")
		}

		if in.ReleaseLabel != nil {
			a.ReleaseLabel = in.ReleaseLabel
		}

		if in.Architecture != nil {
			a.Architecture = in.Architecture
		}

		if in.InitialCapacity != nil {
			a.InitialCapacity = in.InitialCapacity
		}

		if in.MaximumCapacity != nil {
			a.MaximumCapacity = in.MaximumCapacity
		}

		if in.AutoStartConfiguration != nil {
			a.AutoStartConfiguration = in.AutoStartConfiguration
		}

		if in.AutoStopConfiguration != nil {
			a.AutoStopConfiguration = in.AutoStopConfiguration
		}

		if in.NetworkConfiguration != nil {
			a.NetworkConfiguration = in.NetworkConfiguration
		}

		if e := checkApplication(a); e != nil {
			return e
		}

		a.UpdatedAt = common.SecondsMilli(time.Now())
		if e := storage.Write(tx, appKey(scope, common.Value(in.ApplicationId)), a); e != nil {
			return e
		}

		out = &api.UpdateApplicationResponse{Application: a}
		return storage.Write(tx, tk, tokenRecord[api.UpdateApplicationResponse]{Hash: hash, Output: *out})
	})
	return out, err
}

func (s *Service) StartApplication(
	ctx context.Context,
	in *api.StartApplicationRequest,
) (*api.StartApplicationResponse, error) {
	err := s.applicationTransaction(
		ctx,
		in.ApplicationId,
		func(_ storage.WriteTx, _ request.Scope, a *api.Application) error {
			if !slices.Contains([]string{stateCreated, stateStopped, stateStarted}, common.Value(a.State)) {
				return conflict("The application cannot be started in its current state.")
			}

			a.State = new(stateStarted)
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.StartApplicationResponse{}, nil
}

func (s *Service) StopApplication(
	ctx context.Context,
	in *api.StopApplicationRequest,
) (*api.StopApplicationResponse, error) {
	err := s.applicationTransaction(
		ctx,
		in.ApplicationId,
		func(tx storage.WriteTx, scope request.Scope, a *api.Application) error {
			busy, e := hasActiveJobs(tx, scope, common.Value(a.ApplicationId))
			if e != nil {
				return e
			}

			if busy {
				return conflict("Cancel or finish active job runs before stopping the application.")
			}

			a.State = new(stateStopped)
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.StopApplicationResponse{}, nil
}

func (s *Service) DeleteApplication(
	ctx context.Context,
	in *api.DeleteApplicationRequest,
) (*api.DeleteApplicationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, e := readApplication(tx, scope, in.ApplicationId)
		if e != nil {
			return e
		}

		if !slices.Contains([]string{stateCreated, stateStopped}, common.Value(a.State)) {
			return conflict("Stop the application before deleting it.")
		}

		busy, e := hasActiveJobs(tx, scope, common.Value(a.ApplicationId))
		if e != nil {
			return e
		}

		if busy {
			return conflict("The application has active job runs.")
		}

		// TERMINATED is synchronous: remove the application and its job history together.
		for _, k := range tx.Keys(scope.Prefix(serviceName, "jobrun") + common.Value(a.ApplicationId) + "/") {
			tx.Delete(k)
		}

		tx.Delete(appKey(scope, common.Value(a.ApplicationId)))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteApplicationResponse{}, nil
}

func (s *Service) ListApplications(
	ctx context.Context,
	in *api.ListApplicationsRequest,
) (*api.ListApplicationsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	if validationErr := checkStates(in.States, applicationStates); validationErr != nil {
		return nil, validationErr
	}

	all := []api.ApplicationSummary{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rows, e := storage.Records[api.Application](tx, scope.Prefix(serviceName, "application"))
		if e != nil {
			return e
		}

		for _, a := range rows {
			if len(in.States) > 0 && !slices.Contains(in.States, common.Value(a.State)) {
				continue
			}

			all = append(all, api.ApplicationSummary{
				Id:           a.ApplicationId,
				Name:         a.Name,
				Arn:          a.Arn,
				ReleaseLabel: a.ReleaseLabel,
				Type:         a.Type,
				State:        a.State,
				StateDetails: a.StateDetails,
				CreatedAt:    a.CreatedAt,
				UpdatedAt:    a.UpdatedAt,
				Architecture: a.Architecture,
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		all,
		func(a api.ApplicationSummary) string { return common.Value(a.Id) },
		in.NextToken,
		limit,
		awserror.Validation("Invalid nextToken."),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListApplicationsResponse{Applications: page, NextToken: next}, nil
}
