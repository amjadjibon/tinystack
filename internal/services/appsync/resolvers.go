package appsync

import (
	"context"
	"slices"

	"github.com/vektah/gqlparser/v2/ast"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/appsync"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindUnit     = "UNIT"
	kindPipeline = "PIPELINE"
	runtimeJS    = "APPSYNC_JS"
)

// code is how a resolver or function computes its request and response:
// APPSYNC_JS code, VTL mapping templates, or, for a unit resolver on a Lambda
// data source, neither (a direct Lambda resolver).
type code struct {
	runtime           *api.AppSyncRuntime
	source            *string
	request, response *string
	maxBatchSize      *int64
}

func (c code) check() error {
	switch {
	case c.runtime != nil && (common.Value(c.runtime.Name) != runtimeJS ||
		common.Value(c.runtime.RuntimeVersion) != "1.0.0"):
		return badRequest("runtime must be %s 1.0.0.", runtimeJS)
	case (c.runtime != nil) != (c.source != nil):
		return badRequest("code and runtime must be set together.")
	case c.runtime != nil && (c.request != nil || c.response != nil):
		return badRequest("Mapping templates cannot be used with the %s runtime.", runtimeJS)
	case common.Value(c.maxBatchSize) < 0 || common.Value(c.maxBatchSize) > 2000:
		return badRequest("maxBatchSize must be between 0 and 2000.")
	}

	if c.source != nil {
		if _, err := compile(*c.source); err != nil {
			return badRequest("The code contains one or more errors: %s", err.Error())
		}
	}

	return nil
}

func resolverKey(scope request.Scope, apiID, typeName, field string) string {
	return owned(scope, kindResolver, apiID, typeName+"/"+field)
}

// checkResolver validates r against the API's schema, data sources, and
// functions.
func checkResolver(tx storage.ReadTx, scope request.Scope, apiID *string, r *api.Resolver) error {
	c := code{r.Runtime, r.Code, r.RequestMappingTemplate, r.ResponseMappingTemplate, r.MaxBatchSize}
	if err := c.check(); err != nil {
		return err
	}

	schema, ok, err := activeSchema(tx, scope, apiID)
	if err != nil {
		return err
	}

	var d *ast.Definition
	if ok {
		d = schema.Types[*r.TypeName]
	}

	switch {
	case d == nil || d.BuiltIn:
		return notFound("No type named %s found in the schema.", *r.TypeName)
	case d.Fields.ForName(*r.FieldName) == nil:
		return notFound("No field named %s found on type %s.", *r.FieldName, *r.TypeName)
	}

	switch common.Value(r.Kind) {
	case kindUnit:
		if r.PipelineConfig != nil {
			return badRequest("A UNIT resolver cannot have a pipelineConfig.")
		}

		_, err = loadDataSource(tx, scope, apiID, r.DataSourceName)
		return err
	case kindPipeline:
		if r.DataSourceName != nil {
			return badRequest("A PIPELINE resolver cannot have a data source.")
		}

		for _, id := range common.OrElse(r.PipelineConfig, &api.PipelineConfig{}).Functions {
			if _, err = loadFunction(tx, scope, apiID, &id); err != nil {
				return err
			}
		}

		return nil
	default:
		return badRequest("kind must be UNIT or PIPELINE.")
	}
}

// putResolver validates r and stores it, replacing the resolver of its field
// only when replace is set.
func (s *Service) putResolver(ctx context.Context, apiID *string, r *api.Resolver, replace bool) error {
	r.Kind = common.OrElse(r.Kind, new(kindUnit))
	return s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		key := resolverKey(scope, common.Value(apiID), common.Value(r.TypeName), common.Value(r.FieldName))
		_, exists := tx.Get(key)
		switch {
		case replace && !exists:
			return notFound("No resolver found for %s.%s.", *r.TypeName, *r.FieldName)
		case !replace && exists:
			return badRequest("Only one resolver is allowed per field.")
		}

		if err := checkResolver(tx, scope, apiID, r); err != nil {
			return err
		}

		r.ResolverArn = new(apiARN(scope, *apiID) + "/types/" + *r.TypeName + "/resolvers/" + *r.FieldName)
		return storage.Write(tx, key, r)
	})
}

func (s *Service) CreateResolver(
	ctx context.Context,
	in *api.CreateResolverRequest,
) (*api.CreateResolverResponse, error) {
	r := api.Resolver{
		TypeName: in.TypeName, FieldName: in.FieldName, DataSourceName: in.DataSourceName,
		RequestMappingTemplate: in.RequestMappingTemplate, ResponseMappingTemplate: in.ResponseMappingTemplate,
		Kind: in.Kind, PipelineConfig: in.PipelineConfig, MaxBatchSize: in.MaxBatchSize, Runtime: in.Runtime,
		Code: in.Code,
	}
	if err := s.putResolver(ctx, in.ApiId, &r, false); err != nil {
		return nil, err
	}

	return &api.CreateResolverResponse{Resolver: &r}, nil
}

func (s *Service) UpdateResolver(
	ctx context.Context,
	in *api.UpdateResolverRequest,
) (*api.UpdateResolverResponse, error) {
	r := api.Resolver{
		TypeName: in.TypeName, FieldName: in.FieldName, DataSourceName: in.DataSourceName,
		RequestMappingTemplate: in.RequestMappingTemplate, ResponseMappingTemplate: in.ResponseMappingTemplate,
		Kind: in.Kind, PipelineConfig: in.PipelineConfig, MaxBatchSize: in.MaxBatchSize, Runtime: in.Runtime,
		Code: in.Code,
	}
	if err := s.putResolver(ctx, in.ApiId, &r, true); err != nil {
		return nil, err
	}

	return &api.UpdateResolverResponse{Resolver: &r}, nil
}

func (s *Service) GetResolver(ctx context.Context, in *api.GetResolverRequest) (*api.GetResolverResponse, error) {
	out := &api.GetResolverResponse{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		if _, err := loadAPI(tx, scope, in.ApiId); err != nil {
			return err
		}

		r, ok, err := storage.Read[api.Resolver](tx, resolverKey(scope, *in.ApiId, common.Value(in.TypeName),
			common.Value(in.FieldName)))
		if err != nil || !ok {
			return common.ErrorsOr(err, notFound("No resolver found for %s.%s.", common.Value(in.TypeName),
				common.Value(in.FieldName)))
		}

		out.Resolver = r
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteResolver(
	ctx context.Context,
	in *api.DeleteResolverRequest,
) (*api.DeleteResolverResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := loadAPI(tx, scope, in.ApiId); err != nil {
			return err
		}

		key := resolverKey(scope, *in.ApiId, common.Value(in.TypeName), common.Value(in.FieldName))
		if _, ok := tx.Get(key); !ok {
			return notFound("No resolver found for %s.%s.", common.Value(in.TypeName), common.Value(in.FieldName))
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteResolverResponse{}, nil
}

// listResolvers pages through the resolvers under the API's prefix after it
// that keep reports true for.
func (s *Service) listResolvers(
	ctx context.Context,
	apiID *string,
	after string,
	keep func(api.Resolver) bool,
	token *string,
	limit *int64,
) (resolvers []api.Resolver, next *string, err error) {
	err = s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		if _, loadErr := loadAPI(tx, scope, apiID); loadErr != nil {
			return loadErr
		}

		all, readErr := records[api.Resolver](tx, owned(scope, kindResolver, *apiID, after))
		if readErr != nil {
			return readErr
		}

		all = slices.DeleteFunc(all, func(r api.Resolver) bool { return !keep(r) })
		resolvers, next, readErr = page(all, func(r api.Resolver) string { return *r.TypeName + "." + *r.FieldName },
			token, limit)
		return readErr
	})

	return resolvers, next, err
}

func (s *Service) ListResolvers(ctx context.Context, in *api.ListResolversRequest) (*api.ListResolversResponse, error) {
	out := &api.ListResolversResponse{}
	var err error
	out.Resolvers, out.NextToken, err = s.listResolvers(ctx, in.ApiId, common.Value(in.TypeName)+"/",
		func(api.Resolver) bool { return true }, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ListResolversByFunction(
	ctx context.Context,
	in *api.ListResolversByFunctionRequest,
) (*api.ListResolversByFunctionResponse, error) {
	out := &api.ListResolversByFunctionResponse{}
	var err error
	out.Resolvers, out.NextToken, err = s.listResolvers(ctx, in.ApiId, "", func(r api.Resolver) bool {
		return r.PipelineConfig != nil && slices.Contains(r.PipelineConfig.Functions, common.Value(in.FunctionId))
	}, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	return out, nil
}
