package apigateway

import (
	"context"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var endpointTypes = []string{"EDGE", "REGIONAL", "PRIVATE"}

// checkAPI validates the settings CreateRestApi and UpdateRestApi share.
func checkAPI(a *api.RestApi) error {
	switch {
	case common.Value(a.Name) == "":
		return badRequest("Name is required.")
	case a.ApiKeySource != nil && *a.ApiKeySource != "HEADER" && *a.ApiKeySource != "AUTHORIZER":
		return badRequest("ApiKeySource must be HEADER or AUTHORIZER.")
	case a.MinimumCompressionSize != nil && (*a.MinimumCompressionSize < 0 || *a.MinimumCompressionSize > 10485760):
		return badRequest("MinimumCompressionSize must be between 0 and 10485760.")
	}

	if c := a.EndpointConfiguration; c != nil {
		if len(c.Types) != 1 || !slices.Contains(endpointTypes, c.Types[0]) {
			return badRequest("EndpointConfiguration needs one type: EDGE, REGIONAL, or PRIVATE.")
		}

		if ip := common.Value(c.IpAddressType); ip != "" && ip != "ipv4" && ip != "dualstack" {
			return badRequest("IpAddressType must be ipv4 or dualstack.")
		}
	}

	return nil
}

func loadAPI(tx storage.ReadTx, scope request.Scope, id *string) (*restAPI, error) {
	a, ok, err := storage.Read[restAPI](tx, apiKey(scope, common.Value(id)))
	if err != nil || !ok {
		return nil, common.ErrorsOr(
			err,
			notFound("Invalid API identifier specified %s:%s", scope.AccountID, common.Value(id)),
		)
	}

	return a, nil
}

func (s *Service) CreateRestApi(ctx context.Context, in *api.CreateRestApiRequest) (*api.RestApi, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	id, root := common.RandomID(10), common.RandomID(10)
	a := &restAPI{
		API: api.RestApi{
			Id:                     new(id),
			Name:                   in.Name,
			Description:            in.Description,
			Version:                in.Version,
			BinaryMediaTypes:       in.BinaryMediaTypes,
			MinimumCompressionSize: in.MinimumCompressionSize,
			ApiKeySource: common.OrElse(
				in.ApiKeySource,
				new("HEADER"),
			),
			Policy:      in.Policy,
			CreatedDate: common.Seconds(s.now()),
			EndpointConfiguration: common.OrElse(in.EndpointConfiguration, &api.EndpointConfiguration{
				Types: []string{"EDGE"}, IpAddressType: new("ipv4"),
			}),
			DisableExecuteApiEndpoint: new(common.Value(in.DisableExecuteApiEndpoint)),
			RootResourceId:            new(root),
			Tags:                      common.CloneTags(in.Tags),
			ApiStatus:                 new("AVAILABLE"),
		},
		Resources:   map[string]*api.Resource{root: {Id: new(root), Path: new("/")}},
		Authorizers: map[string]*api.Authorizer{},
	}
	if a.API.EndpointConfiguration.IpAddressType == nil {
		a.API.EndpointConfiguration.IpAddressType = new("ipv4")
	}

	if checkErr := checkAPI(&a.API); checkErr != nil {
		return nil, checkErr
	}

	if checkErr := checkPolicy(m.Scope, &a.API); checkErr != nil {
		return nil, checkErr
	}

	if checkErr := checkTags(in.Tags); checkErr != nil {
		return nil, checkErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if writeErr := storage.Write(tx, apiKey(m.Scope, id), a); writeErr != nil {
			return writeErr
		}

		return storage.Write(tx, indexKey(id), location{Account: m.AccountID, Region: m.Region})
	})
	if err != nil {
		return nil, err
	}

	return &a.API, nil
}

func (s *Service) GetRestApi(ctx context.Context, in *api.GetRestApiRequest) (*api.RestApi, error) {
	var out *api.RestApi
	err := s.viewAPI(ctx, in.RestApiId, func(_ storage.ReadTx, _ request.Scope, a *restAPI) error {
		out = &a.API
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// viewAPI runs fn on a REST API in a read transaction.
func (s *Service) viewAPI(
	ctx context.Context,
	id *string,
	fn func(tx storage.ReadTx, scope request.Scope, a *restAPI) error,
) error {
	m, err := request.Regional(ctx)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error {
		a, loadErr := loadAPI(tx, m.Scope, id)
		if loadErr != nil {
			return loadErr
		}

		return fn(tx, m.Scope, a)
	})
}

// changeAPI runs fn on a REST API and stores it.
func (s *Service) changeAPI(
	ctx context.Context,
	id *string,
	fn func(tx storage.WriteTx, scope request.Scope, a *restAPI) error,
) error {
	m, err := request.Regional(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAPI(tx, m.Scope, id)
		if loadErr != nil {
			return loadErr
		}

		if fnErr := fn(tx, m.Scope, a); fnErr != nil {
			return fnErr
		}

		return storage.Write(tx, apiKey(m.Scope, *a.API.Id), a)
	})
}

func (s *Service) GetRestApis(ctx context.Context, in *api.GetRestApisRequest) (*api.RestApis, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.RestApis{Items: []api.RestApi{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, readErr := storage.Records[restAPI](tx, m.Prefix(service, kindAPI))
		if readErr != nil {
			return readErr
		}

		items := make([]api.RestApi, 0, len(all))
		for _, a := range all {
			items = append(items, a.API)
		}

		items, out.Position, readErr = page(items, func(a api.RestApi) string { return *a.Id }, in.Position, in.Limit)
		out.Items = append(out.Items, items...)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateRestApi(ctx context.Context, in *api.UpdateRestApiRequest) (*api.RestApi, error) {
	var out *api.RestApi
	err := s.changeAPI(ctx, in.RestApiId, func(_ storage.WriteTx, scope request.Scope, a *restAPI) error {
		if patchErr := patch(&a.API, in.PatchOperations, "name", "description", "version", "binaryMediaTypes",
			"minimumCompressionSize", "apiKeySource", "policy", "disableExecuteApiEndpoint",
			"endpointConfiguration"); patchErr != nil {
			return patchErr
		}

		out = &a.API
		if checkErr := checkAPI(&a.API); checkErr != nil {
			return checkErr
		}

		return checkPolicy(scope, &a.API)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteRestApi(ctx context.Context, in *api.DeleteRestApiRequest) (*api.DeleteRestApiOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.RestApiId)
		if loadErr != nil {
			return loadErr
		}

		for _, kind := range []string{kindDeployment, kindStage} {
			for _, k := range tx.Keys(m.Prefix(service, kind) + *a.API.Id + "/") {
				tx.Delete(k)
			}
		}

		tx.Delete(apiKey(m.Scope, *a.API.Id))
		tx.Delete(indexKey(*a.API.Id))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteRestApiOutput{}, nil
}
