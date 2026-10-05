package appsync

import (
	"context"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/appsync"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	authAPIKey  = "API_KEY"
	authIAM     = "AWS_IAM"
	authCognito = "AMAZON_COGNITO_USER_POOLS"
	authOIDC    = "OPENID_CONNECT"
	authLambda  = "AWS_LAMBDA"
	typeGraphQL = "GRAPHQL"
)

// view runs fn in a read transaction in the caller's account and region.
func (s *Service) view(ctx context.Context, fn func(storage.ReadTx, request.Scope) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error { return fn(tx, scope) })
}

// update runs fn in a write transaction in the caller's account and region.
func (s *Service) update(ctx context.Context, fn func(storage.WriteTx, request.Scope) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error { return fn(tx, scope) })
}

// checkProvider validates an authorization type and the configuration it
// needs.
func checkProvider(kind *string, pool, oidc, authorizer bool) error {
	switch common.Value(kind) {
	case authAPIKey, authIAM:
	case authCognito:
		if !pool {
			return badRequest("userPoolConfig is required for %s.", authCognito)
		}
	case authOIDC:
		if !oidc {
			return badRequest("openIDConnectConfig is required for %s.", authOIDC)
		}
	case authLambda:
		if !authorizer {
			return badRequest("lambdaAuthorizerConfig is required for %s.", authLambda)
		}
	default:
		return badRequest("Invalid authenticationType %s.", common.Value(kind))
	}

	return nil
}

// checkAPI validates the settings CreateGraphqlApi and UpdateGraphqlApi share.
func checkAPI(a *api.GraphqlApi) error {
	if common.Value(a.Name) == "" {
		return badRequest("name is required.")
	}

	err := checkProvider(a.AuthenticationType, a.UserPoolConfig != nil, a.OpenIDConnectConfig != nil,
		a.LambdaAuthorizerConfig != nil)
	if err != nil {
		return err
	}

	seen := []string{*a.AuthenticationType}
	for _, p := range a.AdditionalAuthenticationProviders {
		err = checkProvider(p.AuthenticationType, p.UserPoolConfig != nil, p.OpenIDConnectConfig != nil,
			p.LambdaAuthorizerConfig != nil)
		if err != nil {
			return err
		}

		if slices.Contains(seen, *p.AuthenticationType) && *p.AuthenticationType != authCognito &&
			*p.AuthenticationType != authOIDC {
			return badRequest("Authentication type %s is used more than once.", *p.AuthenticationType)
		}

		seen = append(seen, *p.AuthenticationType)
	}

	if l := a.LogConfig; l != nil {
		if !slices.Contains([]string{"NONE", "ERROR", "ALL", "INFO", "DEBUG"}, common.Value(l.FieldLogLevel)) ||
			common.Value(l.CloudWatchLogsRoleArn) == "" {
			return badRequest("logConfig needs a valid fieldLogLevel and a cloudWatchLogsRoleArn.")
		}
	}

	switch {
	case !slices.Contains([]string{"ENABLED", "DISABLED"}, common.Value(a.IntrospectionConfig)):
		return badRequest("introspectionConfig must be ENABLED or DISABLED.")
	case common.Value(a.QueryDepthLimit) < 0 || common.Value(a.QueryDepthLimit) > 75:
		return badRequest("queryDepthLimit must be between 0 and 75.")
	case common.Value(a.ResolverCountLimit) < 0 || common.Value(a.ResolverCountLimit) > 10000:
		return badRequest("resolverCountLimit must be between 0 and 10000.")
	}

	return nil
}

// configure copies the settings CreateGraphqlApiRequest and
// UpdateGraphqlApiRequest share into a, keeping a's values for the optional
// settings in left out.
func configure(a *api.GraphqlApi, in *api.CreateGraphqlApiRequest) {
	a.Name, a.AuthenticationType = in.Name, in.AuthenticationType
	a.UserPoolConfig, a.OpenIDConnectConfig = in.UserPoolConfig, in.OpenIDConnectConfig
	a.LambdaAuthorizerConfig, a.LogConfig = in.LambdaAuthorizerConfig, in.LogConfig
	a.AdditionalAuthenticationProviders = in.AdditionalAuthenticationProviders
	a.XrayEnabled, a.OwnerContact = common.OrElse(in.XrayEnabled, a.XrayEnabled), common.OrElse(in.OwnerContact,
		a.OwnerContact)
	a.IntrospectionConfig = common.OrElse(in.IntrospectionConfig, a.IntrospectionConfig)
	a.QueryDepthLimit = common.OrElse(in.QueryDepthLimit, a.QueryDepthLimit)
	a.ResolverCountLimit = common.OrElse(in.ResolverCountLimit, a.ResolverCountLimit)
}

func (s *Service) CreateGraphqlApi(
	ctx context.Context,
	in *api.CreateGraphqlApiRequest,
) (*api.CreateGraphqlApiResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if checkErr := checkTags(in.Tags); checkErr != nil {
		return nil, checkErr
	}

	switch {
	case common.Value(in.ApiType) != "" && *in.ApiType != typeGraphQL:
		return nil, badRequest("TinyStack supports GRAPHQL APIs only, not %s.", *in.ApiType)
	case common.Value(in.Visibility) != "" && *in.Visibility != "GLOBAL":
		return nil, badRequest("TinyStack supports GLOBAL APIs only, not %s.", *in.Visibility)
	}

	id := common.RandomID(26)
	scheme, h := host(m.Endpoint, id)
	a := &api.GraphqlApi{
		ApiId: new(
			id,
		),
		Arn:                 new(apiARN(m.Scope, id)),
		Uris:                map[string]string{typeGraphQL: scheme + "://" + h + "/graphql"},
		Dns:                 map[string]string{typeGraphQL: h},
		Visibility:          new("GLOBAL"),
		ApiType:             new(typeGraphQL),
		Owner:               new(m.AccountID),
		XrayEnabled:         new(false),
		IntrospectionConfig: new("ENABLED"),
		QueryDepthLimit:     new(int64(0)),
		ResolverCountLimit:  new(int64(0)),
		Tags:                common.CloneTags(in.Tags),
	}
	configure(a, in)
	if checkErr := checkAPI(a); checkErr != nil {
		return nil, checkErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if writeErr := storage.Write(tx, m.Key(service, kindAPI, id), a); writeErr != nil {
			return writeErr
		}

		return storage.Write(tx, indexKey(id), location{Account: m.AccountID, Region: m.Region})
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateGraphqlApiResponse{GraphqlApi: a}, nil
}

func (s *Service) GetGraphqlApi(ctx context.Context, in *api.GetGraphqlApiRequest) (*api.GetGraphqlApiResponse, error) {
	out := &api.GetGraphqlApiResponse{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var loadErr error
		out.GraphqlApi, loadErr = loadAPI(tx, scope, in.ApiId)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ListGraphqlApis(
	ctx context.Context,
	in *api.ListGraphqlApisRequest,
) (*api.ListGraphqlApisResponse, error) {
	out := &api.ListGraphqlApisResponse{GraphqlApis: []api.GraphqlApi{}}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		all, readErr := records[api.GraphqlApi](tx, scope.Prefix(service, kindAPI))
		if readErr != nil {
			return readErr
		}

		out.GraphqlApis, out.NextToken, readErr = page(all, func(a api.GraphqlApi) string { return *a.ApiId },
			in.NextToken, in.MaxResults)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateGraphqlApi(
	ctx context.Context,
	in *api.UpdateGraphqlApiRequest,
) (*api.UpdateGraphqlApiResponse, error) {
	out := &api.UpdateGraphqlApiResponse{}
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		a, loadErr := loadAPI(tx, scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		configure(a, &api.CreateGraphqlApiRequest{
			Name: in.Name, LogConfig: in.LogConfig, AuthenticationType: in.AuthenticationType,
			UserPoolConfig: in.UserPoolConfig, OpenIDConnectConfig: in.OpenIDConnectConfig,
			AdditionalAuthenticationProviders: in.AdditionalAuthenticationProviders, XrayEnabled: in.XrayEnabled,
			LambdaAuthorizerConfig: in.LambdaAuthorizerConfig, OwnerContact: in.OwnerContact,
			IntrospectionConfig: in.IntrospectionConfig, QueryDepthLimit: in.QueryDepthLimit,
			ResolverCountLimit: in.ResolverCountLimit,
		})
		if checkErr := checkAPI(a); checkErr != nil {
			return checkErr
		}

		out.GraphqlApi = a
		return storage.Write(tx, scope.Key(service, kindAPI, *a.ApiId), a)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteGraphqlApi(
	ctx context.Context,
	in *api.DeleteGraphqlApiRequest,
) (*api.DeleteGraphqlApiResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		a, loadErr := loadAPI(tx, scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		for _, kind := range []string{kindAPIKey, kindDataSource, kindFunction, kindResolver} {
			for _, k := range tx.Keys(owned(scope, kind, *a.ApiId, "")) {
				tx.Delete(k)
			}
		}

		tx.Delete(scope.Key(service, kindSchema, *a.ApiId))
		tx.Delete(scope.Key(service, kindAPI, *a.ApiId))
		tx.Delete(indexKey(*a.ApiId))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteGraphqlApiResponse{}, nil
}

// records reads the records under prefix as values, in key order.
func records[T any](tx storage.ReadTx, prefix string) ([]T, error) {
	all, err := storage.Records[T](tx, prefix)
	if err != nil {
		return nil, err
	}

	out := make([]T, 0, len(all))
	for _, r := range all {
		out = append(out, *r)
	}

	return out, nil
}
