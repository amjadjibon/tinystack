package appsync

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/appsync"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func loadFunction(tx storage.ReadTx, scope request.Scope, apiID, id *string) (*api.FunctionConfiguration, error) {
	if _, err := loadAPI(tx, scope, apiID); err != nil {
		return nil, err
	}

	f, ok, err := storage.Read[api.FunctionConfiguration](tx,
		owned(scope, kindFunction, *apiID, common.Value(id)))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, notFound("Function %s not found.", common.Value(id)))
	}

	return f, nil
}

// putFunction validates f and stores it under the API apiID names, replacing
// the function with its ID, which must exist, when replace is set.
func (s *Service) putFunction(ctx context.Context, apiID *string, f *api.FunctionConfiguration, replace bool) error {
	c := code{f.Runtime, f.Code, f.RequestMappingTemplate, f.ResponseMappingTemplate, f.MaxBatchSize}
	if err := c.check(); err != nil {
		return err
	}

	if err := checkName("Function name", common.Value(f.Name)); err != nil {
		return err
	}

	return s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if replace {
			if _, err := loadFunction(tx, scope, apiID, f.FunctionId); err != nil {
				return err
			}
		}

		if _, err := loadDataSource(tx, scope, apiID, f.DataSourceName); err != nil {
			return err
		}

		f.FunctionArn = new(apiARN(scope, *apiID) + "/functions/" + *f.FunctionId)
		return storage.Write(tx, owned(scope, kindFunction, *apiID, *f.FunctionId), f)
	})
}

func (s *Service) CreateFunction(
	ctx context.Context,
	in *api.CreateFunctionRequest,
) (*api.CreateFunctionResponse, error) {
	f := &api.FunctionConfiguration{
		FunctionId: new(common.RandomID(26)), Name: in.Name, Description: in.Description,
		DataSourceName: in.DataSourceName, RequestMappingTemplate: in.RequestMappingTemplate,
		ResponseMappingTemplate: in.ResponseMappingTemplate, FunctionVersion: in.FunctionVersion,
		MaxBatchSize: in.MaxBatchSize, Runtime: in.Runtime, Code: in.Code,
	}
	if err := s.putFunction(ctx, in.ApiId, f, false); err != nil {
		return nil, err
	}

	return &api.CreateFunctionResponse{FunctionConfiguration: f}, nil
}

func (s *Service) UpdateFunction(
	ctx context.Context,
	in *api.UpdateFunctionRequest,
) (*api.UpdateFunctionResponse, error) {
	f := &api.FunctionConfiguration{
		FunctionId: in.FunctionId, Name: in.Name, Description: in.Description, DataSourceName: in.DataSourceName,
		RequestMappingTemplate: in.RequestMappingTemplate, ResponseMappingTemplate: in.ResponseMappingTemplate,
		FunctionVersion: in.FunctionVersion, MaxBatchSize: in.MaxBatchSize, Runtime: in.Runtime, Code: in.Code,
	}
	if err := s.putFunction(ctx, in.ApiId, f, true); err != nil {
		return nil, err
	}

	return &api.UpdateFunctionResponse{FunctionConfiguration: f}, nil
}

func (s *Service) GetFunction(ctx context.Context, in *api.GetFunctionRequest) (*api.GetFunctionResponse, error) {
	out := &api.GetFunctionResponse{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		out.FunctionConfiguration, err = loadFunction(tx, scope, in.ApiId, in.FunctionId)
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteFunction(
	ctx context.Context,
	in *api.DeleteFunctionRequest,
) (*api.DeleteFunctionResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := loadFunction(tx, scope, in.ApiId, in.FunctionId); err != nil {
			return err
		}

		tx.Delete(owned(scope, kindFunction, *in.ApiId, *in.FunctionId))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteFunctionResponse{}, nil
}

func (s *Service) ListFunctions(ctx context.Context, in *api.ListFunctionsRequest) (*api.ListFunctionsResponse, error) {
	out := &api.ListFunctionsResponse{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		if _, err := loadAPI(tx, scope, in.ApiId); err != nil {
			return err
		}

		all, err := records[api.FunctionConfiguration](tx, owned(scope, kindFunction, *in.ApiId, ""))
		if err != nil {
			return err
		}

		out.Functions, out.NextToken, err = page(all, func(f api.FunctionConfiguration) string {
			return *f.FunctionId
		}, in.NextToken, in.MaxResults)
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
