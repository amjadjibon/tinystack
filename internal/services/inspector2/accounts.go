package inspector2

import (
	"context"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/inspector2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type accountRecord struct {
	Types         map[string]string
	Configuration api.GetConfigurationResponse
}

func defaultAccount() *accountRecord {
	return &accountRecord{Types: map[string]string{
		resourceEC2:            statusDisabled,
		resourceECR:            statusDisabled,
		resourceLambda:         statusDisabled,
		resourceLambdaCode:     statusDisabled,
		resourceCodeRepository: statusDisabled,
	}, Configuration: api.GetConfigurationResponse{
		EcrConfiguration: &api.EcrConfigurationState{RescanDurationState: &api.EcrRescanDurationState{
			RescanDuration: new("DAYS_14"), Status: new(statusSuccess),
		}},
		Ec2Configuration: &api.Ec2ConfigurationState{ScanModeState: &api.Ec2ScanModeState{
			ScanMode: new("EC2_HYBRID"), ScanModeStatus: new(statusSuccess),
		}},
	}}
}

func loadAccount(tx storage.ReadTx, scope request.Scope) (*accountRecord, error) {
	r, found, err := storage.Read[accountRecord](tx, scope.Key(serviceName, kindAccount, accountSettings))
	if err != nil {
		return nil, err
	}

	if !found {
		return defaultAccount(), nil
	}

	return r, nil
}

func (r *accountRecord) status() string {
	for _, s := range r.Types {
		if s == statusEnabled {
			return statusEnabled
		}
	}

	return statusDisabled
}

func (r *accountRecord) account(id string) api.Account {
	return api.Account{AccountId: new(id), Status: new(r.status()), ResourceStatus: &api.ResourceStatus{
		Ec2: new(r.Types[resourceEC2]), Ecr: new(r.Types[resourceECR]), Lambda: new(r.Types[resourceLambda]),
		LambdaCode: new(r.Types[resourceLambdaCode]), CodeRepository: new(r.Types[resourceCodeRepository]),
	}}
}

type enableToken struct {
	Types    []string
	Accounts []api.Account
}

func (s *Service) setScanning(
	ctx context.Context,
	ids, types []string,
	status string,
	token *string,
) ([]api.Account, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = ownAccount(scope, ids)
	if err != nil {
		return nil, err
	}

	var out []api.Account
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if token != nil {
			previous, found, e := storage.Read[enableToken](tx, scope.Key(serviceName, "enable-token", *token))
			if e != nil {
				return e
			}

			if found {
				if !slices.Equal(previous.Types, types) {
					return awserror.Validation("clientToken was used with different resourceTypes.")
				}

				out = previous.Accounts
				return nil
			}
		}

		r, e := loadAccount(tx, scope)
		if e != nil {
			return e
		}

		if len(types) == 0 {
			types = []string{resourceEC2, resourceECR, resourceLambda, resourceLambdaCode, resourceCodeRepository}
		}

		for _, t := range types {
			r.Types[t] = status
		}

		out = []api.Account{r.account(scope.AccountID)}
		if token != nil {
			e = storage.Write(
				tx,
				scope.Key(serviceName, "enable-token", *token),
				enableToken{Types: types, Accounts: out},
			)
			if e != nil {
				return e
			}
		}

		return storage.Write(tx, scope.Key(serviceName, kindAccount, accountSettings), r)
	})
	return out, err
}

func (s *Service) Enable(ctx context.Context, in *api.EnableRequest) (*api.EnableResponse, error) {
	accounts, err := s.setScanning(ctx, in.AccountIds, in.ResourceTypes, statusEnabled, in.ClientToken)
	return &api.EnableResponse{Accounts: accounts, FailedAccounts: []api.FailedAccount{}}, err
}

func (s *Service) Disable(ctx context.Context, in *api.DisableRequest) (*api.DisableResponse, error) {
	accounts, err := s.setScanning(ctx, in.AccountIds, in.ResourceTypes, statusDisabled, nil)
	return &api.DisableResponse{Accounts: accounts, FailedAccounts: []api.FailedAccount{}}, err
}

func state(status string) *api.State {
	return &api.State{Status: new(status), ErrorCode: new(""), ErrorMessage: new("")}
}

func (s *Service) BatchGetAccountStatus(ctx context.Context, in *api.BatchGetAccountStatusRequest) (
	*api.BatchGetAccountStatusResponse, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = ownAccount(scope, in.AccountIds)
	if err != nil {
		return nil, err
	}

	out := &api.BatchGetAccountStatusResponse{FailedAccounts: []api.FailedAccount{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := loadAccount(tx, scope)
		if e != nil {
			return e
		}

		out.Accounts = []api.AccountState{{
			AccountId: new(scope.AccountID), State: state(r.status()),
			ResourceState: &api.ResourceState{
				Ec2: state(r.Types[resourceEC2]), Ecr: state(r.Types[resourceECR]),
				Lambda: state(r.Types[resourceLambda]), LambdaCode: state(r.Types[resourceLambdaCode]),
				CodeRepository: state(r.Types[resourceCodeRepository]),
			},
		}}
		return nil
	})
	return out, err
}

func (s *Service) GetConfiguration(ctx context.Context, in *api.GetConfigurationRequest) (
	*api.GetConfigurationResponse, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in.AccountId != nil {
		err = ownAccount(scope, []string{*in.AccountId})
		if err != nil {
			return nil, err
		}
	}

	var out api.GetConfigurationResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := loadAccount(tx, scope)
		if e != nil {
			return e
		}

		out = r.Configuration
		return nil
	})
	return &out, err
}

func (s *Service) UpdateConfiguration(ctx context.Context, in *api.UpdateConfigurationRequest) (
	*api.UpdateConfigurationResponse, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in.AccountId != nil {
		err = ownAccount(scope, []string{*in.AccountId})
		if err != nil {
			return nil, err
		}
	}

	if in.EcrConfiguration == nil && in.Ec2Configuration == nil {
		return nil, awserror.Validation("At least one configuration is required.")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := loadAccount(tx, scope)
		if e != nil {
			return e
		}

		if in.EcrConfiguration != nil {
			r.Configuration.EcrConfiguration.RescanDurationState = &api.EcrRescanDurationState{
				RescanDuration: common.Clone(in.EcrConfiguration.RescanDuration), Status: new(statusSuccess),
				UpdatedAt: new(float64(time.Now().Unix())),
			}
		}

		if in.Ec2Configuration != nil {
			r.Configuration.Ec2Configuration.ScanModeState = &api.Ec2ScanModeState{
				ScanMode: common.Clone(in.Ec2Configuration.ScanMode), ScanModeStatus: new(statusSuccess),
			}
		}

		return storage.Write(tx, scope.Key(serviceName, kindAccount, accountSettings), r)
	})
	return &api.UpdateConfigurationResponse{}, err
}
