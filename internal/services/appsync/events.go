package appsync

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/appsync"
	cognitoapi "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	lambdaapi "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindEventAPI      = "eventapi"
	kindNamespace     = "namespace"
	keyEventChannel   = "channel"
	keyEventOperation = "operation"
	keyMessageType    = "type"
	keyEventErrors    = "errors"
	keyAccountID      = "accountId"
	keyAPIID          = "ApiId"
	keyEventHeaders   = "headers"
)

var eventName = regexp.MustCompile(`^[A-Za-z0-9_\- ]{1,50}$`)

var eventSegment = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,48}[A-Za-z0-9])?$`)

func eventIndexKey(id string) string { return "aws/" + service + "/eventapi/" + id }

func loadEventAPI(tx storage.ReadTx, scope request.Scope, id *string) (*api.Api, error) {
	a, ok, err := storage.Read[api.Api](tx, scope.Key(service, kindEventAPI, common.Value(id)))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, notFound("Event API %s not found.", common.Value(id)))
	}

	return a, nil
}

func loadNamespace(tx storage.ReadTx, scope request.Scope, id, name *string) (*api.ChannelNamespace, error) {
	if _, err := loadEventAPI(tx, scope, id); err != nil {
		return nil, err
	}

	n, ok, err := storage.Read[api.ChannelNamespace](tx, owned(scope, kindNamespace,
		common.Value(id), common.Value(name)))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, notFound("Channel namespace %s not found.", common.Value(name)))
	}

	return n, nil
}

// API keys are shared by both API families; their control planes remain separate.
func keyAPI(tx storage.ReadTx, scope request.Scope, id *string) error {
	if _, ok, err := storage.Read[api.Api](tx, scope.Key(service, kindEventAPI, common.Value(id))); err != nil || ok {
		return err
	}

	_, err := loadAPI(tx, scope, id)
	return err
}

func checkEventModes(modes []api.AuthMode, providers []api.AuthProvider, required bool) error {
	if required && len(modes) == 0 {
		return badRequest("At least one authorization mode is required.")
	}

	seen := map[string]bool{}
	for _, mode := range modes {
		kind := common.Value(mode.AuthType)
		if seen[kind] || !slices.ContainsFunc(providers, func(p api.AuthProvider) bool {
			return common.Value(p.AuthType) == kind
		}) {
			return badRequest("Authorization mode %s must be unique and have a configured provider.", kind)
		}

		seen[kind] = true
	}

	return nil
}

func (s *Service) checkEventConfig(ctx context.Context, cfg *api.EventConfig) error {
	if cfg == nil || len(cfg.AuthProviders) == 0 {
		return badRequest("eventConfig with authProviders is required.")
	}

	seen := map[string]bool{}
	for _, p := range cfg.AuthProviders {
		kind := common.Value(p.AuthType)
		if err := checkProvider(p.AuthType, p.CognitoConfig != nil, p.OpenIDConnectConfig != nil,
			p.LambdaAuthorizerConfig != nil); err != nil {
			return err
		}

		if seen[kind] && kind != authCognito && kind != authOIDC {
			return badRequest("Authentication type %s is used more than once.", kind)
		}

		seen[kind] = true
		if p.CognitoConfig != nil && kind != authCognito || p.OpenIDConnectConfig != nil && kind != authOIDC ||
			p.LambdaAuthorizerConfig != nil && kind != authLambda {
			return badRequest("Provider configuration does not match authType.")
		}

		if p.CognitoConfig != nil {
			if s.pools == nil || common.Value(p.CognitoConfig.UserPoolId) == "" ||
				common.Value(p.CognitoConfig.AwsRegion) == "" {
				return badRequest("A local Cognito user pool and awsRegion are required.")
			}

			scope, err := request.RegionalScope(ctx)
			if err != nil {
				return err
			}

			scope.Region = *p.CognitoConfig.AwsRegion
			poolCtx := request.WithMetadata(ctx, request.Metadata{Scope: scope})
			if _, err := s.pools.DescribeUserPool(poolCtx, &cognitoapi.DescribeUserPoolRequest{
				UserPoolId: p.CognitoConfig.UserPoolId,
			}); err != nil {
				return badRequest("Cognito user pool does not exist: %s", err)
			}

			if pattern := p.CognitoConfig.AppIdClientRegex; pattern != nil {
				if _, err := regexp.Compile(*pattern); err != nil {
					return badRequest("Invalid appIdClientRegex: %s", err)
				}
			}
		}

		if p.OpenIDConnectConfig != nil && common.Value(p.OpenIDConnectConfig.Issuer) == "" {
			return badRequest("An OpenID Connect issuer is required.")
		}

		if p.LambdaAuthorizerConfig != nil {
			if s.functions == nil || common.Value(p.LambdaAuthorizerConfig.AuthorizerUri) == "" {
				return badRequest("A local Lambda authorizer is required.")
			}

			if _, err := s.functions.GetFunction(ctx, &lambdaapi.GetFunctionRequest{
				FunctionName: p.LambdaAuthorizerConfig.AuthorizerUri,
			}); err != nil {
				return badRequest("Lambda authorizer does not exist: %s", err)
			}

			if pattern := p.LambdaAuthorizerConfig.IdentityValidationExpression; pattern != nil {
				if _, err := regexp.Compile(*pattern); err != nil {
					return badRequest("Invalid identityValidationExpression: %s", err)
				}
			}
		}
	}

	for _, modes := range [][]api.AuthMode{
		cfg.ConnectionAuthModes, cfg.DefaultPublishAuthModes,
		cfg.DefaultSubscribeAuthModes,
	} {
		if err := checkEventModes(modes, cfg.AuthProviders, true); err != nil {
			return err
		}
	}

	return nil
}

func (s *Service) CreateApi(ctx context.Context, in *api.CreateApiRequest) (*api.CreateApiResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if !eventName.MatchString(common.Value(in.Name)) {
		return nil, badRequest("name must contain 1 to 50 characters.")
	}

	if checkErr := checkTags(in.Tags); checkErr != nil {
		return nil, checkErr
	}

	if checkErr := s.checkEventConfig(ctx, in.EventConfig); checkErr != nil {
		return nil, checkErr
	}

	id := common.RandomID(26)
	_, h := host(m.Endpoint, id, "appsync-api")
	_, realtime := host(m.Endpoint, id, "appsync-realtime-api")
	a := &api.Api{
		ApiId: new(id), ApiArn: new(apiARN(m.Scope, id)), Name: in.Name,
		OwnerContact: in.OwnerContact, EventConfig: in.EventConfig, Tags: common.CloneTags(in.Tags),
		Created: new(float64(s.now().UnixMilli()) / 1000), Dns: map[string]string{"HTTP": h, typeRealtime: realtime},
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if writeErr := storage.Write(tx, m.Key(service, kindEventAPI, id), a); writeErr != nil {
			return writeErr
		}

		return storage.Write(tx, eventIndexKey(id), location{Account: m.AccountID, Region: m.Region})
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateApiResponse{Api: a}, nil
}

func (s *Service) GetApi(ctx context.Context, in *api.GetApiRequest) (*api.GetApiResponse, error) {
	out := &api.GetApiResponse{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		out.Api, err = loadEventAPI(tx, scope, in.ApiId)
		return err
	})
	return out, err
}

func (s *Service) UpdateApi(ctx context.Context, in *api.UpdateApiRequest) (*api.UpdateApiResponse, error) {
	if !eventName.MatchString(common.Value(in.Name)) {
		return nil, badRequest("name must contain 1 to 50 characters.")
	}

	if in.EventConfig != nil {
		if err := s.checkEventConfig(ctx, in.EventConfig); err != nil {
			return nil, err
		}
	}

	out := &api.UpdateApiResponse{}
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		a, err := loadEventAPI(tx, scope, in.ApiId)
		if err != nil {
			return err
		}

		a.Name, a.OwnerContact = in.Name, common.OrElse(in.OwnerContact, a.OwnerContact)
		a.EventConfig = common.OrElse(in.EventConfig, a.EventConfig)
		namespaces, err := storage.Records[api.ChannelNamespace](tx, owned(scope, kindNamespace, *a.ApiId, ""))
		if err != nil {
			return err
		}

		for _, ns := range namespaces {
			if err := checkNamespace(ns, a.EventConfig); err != nil {
				return err
			}
		}

		out.Api = a
		return storage.Write(tx, scope.Key(service, kindEventAPI, *a.ApiId), a)
	})
	return out, err
}

func (s *Service) DeleteApi(ctx context.Context, in *api.DeleteApiRequest) (*api.DeleteApiResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := loadEventAPI(tx, scope, in.ApiId); err != nil {
			return err
		}

		for _, kind := range []string{kindNamespace, kindAPIKey} {
			for _, key := range tx.Keys(owned(scope, kind, *in.ApiId, "")) {
				tx.Delete(key)
			}
		}

		tx.Delete(scope.Key(service, kindEventAPI, *in.ApiId))
		tx.Delete(eventIndexKey(*in.ApiId))
		return nil
	})
	return &api.DeleteApiResponse{}, err
}

func (s *Service) ListApis(ctx context.Context, in *api.ListApisRequest) (*api.ListApisResponse, error) {
	out := &api.ListApisResponse{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		all, err := records[api.Api](tx, scope.Prefix(service, kindEventAPI))
		if err != nil {
			return err
		}

		out.Apis, out.NextToken, err = page(all, func(a api.Api) string { return *a.ApiId },
			in.NextToken, in.MaxResults)
		return err
	})
	return out, err
}

func checkNamespace(n *api.ChannelNamespace, cfg *api.EventConfig) error {
	if !eventSegment.MatchString(common.Value(n.Name)) {
		return badRequest("Invalid channel namespace name.")
	}

	for _, modes := range [][]api.AuthMode{n.PublishAuthModes, n.SubscribeAuthModes} {
		if err := checkEventModes(modes, cfg.AuthProviders, false); err != nil {
			return err
		}
	}

	if err := checkTags(n.Tags); err != nil {
		return err
	}

	if code := n.CodeHandlers; code != nil {
		if len(*code) < 1 || len(*code) > 32768 {
			return badRequest("codeHandlers must contain 1 to 32768 characters.")
		}

		if err := validateEventHandlers(*code); err != nil {
			return badRequest("Invalid codeHandlers: %s", err)
		}
	}

	return nil
}

func (s *Service) CreateChannelNamespace(ctx context.Context, in *api.CreateChannelNamespaceRequest) (
	*api.CreateChannelNamespaceResponse, error,
) {
	out := &api.CreateChannelNamespaceResponse{}
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		a, err := loadEventAPI(tx, scope, in.ApiId)
		if err != nil {
			return err
		}

		n := &api.ChannelNamespace{
			ApiId:               in.ApiId,
			Name:                in.Name,
			PublishAuthModes:    in.PublishAuthModes,
			SubscribeAuthModes:  in.SubscribeAuthModes,
			CodeHandlers:        in.CodeHandlers,
			Tags:                common.CloneTags(in.Tags),
			ChannelNamespaceArn: new(apiARN(scope, *in.ApiId) + "/channelNamespace/" + common.Value(in.Name)),
			Created: new(
				float64(s.now().UnixMilli()) / 1000,
			),
			LastModified: new(float64(s.now().UnixMilli()) / 1000),
		}

		if err := checkNamespace(n, a.EventConfig); err != nil {
			return err
		}

		key := owned(scope, kindNamespace, *in.ApiId, *in.Name)
		if _, ok, err := storage.Read[api.ChannelNamespace](tx, key); err != nil || ok {
			return common.ErrorsOr(err, awserror.New("ConflictException", "Channel namespace already exists."))
		}

		out.ChannelNamespace = n
		return storage.Write(tx, key, n)
	})
	return out, err
}

func (s *Service) GetChannelNamespace(ctx context.Context, in *api.GetChannelNamespaceRequest) (
	*api.GetChannelNamespaceResponse, error,
) {
	out := &api.GetChannelNamespaceResponse{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		out.ChannelNamespace, err = loadNamespace(tx, scope, in.ApiId, in.Name)
		return err
	})
	return out, err
}

func (s *Service) UpdateChannelNamespace(ctx context.Context, in *api.UpdateChannelNamespaceRequest) (
	*api.UpdateChannelNamespaceResponse, error,
) {
	out := &api.UpdateChannelNamespaceResponse{}
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		n, err := loadNamespace(tx, scope, in.ApiId, in.Name)
		if err != nil {
			return err
		}

		a, err := loadEventAPI(tx, scope, in.ApiId)
		if err != nil {
			return err
		}

		n.PublishAuthModes, n.SubscribeAuthModes = in.PublishAuthModes, in.SubscribeAuthModes
		n.CodeHandlers = in.CodeHandlers
		n.LastModified = new(float64(s.now().UnixMilli()) / 1000)
		if err := checkNamespace(n, a.EventConfig); err != nil {
			return err
		}

		out.ChannelNamespace = n
		return storage.Write(tx, owned(scope, kindNamespace, *in.ApiId, *in.Name), n)
	})
	return out, err
}

func (s *Service) DeleteChannelNamespace(ctx context.Context, in *api.DeleteChannelNamespaceRequest) (
	*api.DeleteChannelNamespaceResponse, error,
) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := loadNamespace(tx, scope, in.ApiId, in.Name); err != nil {
			return err
		}

		tx.Delete(owned(scope, kindNamespace, *in.ApiId, *in.Name))
		return nil
	})
	return &api.DeleteChannelNamespaceResponse{}, err
}

func (s *Service) ListChannelNamespaces(ctx context.Context, in *api.ListChannelNamespacesRequest) (
	*api.ListChannelNamespacesResponse, error,
) {
	out := &api.ListChannelNamespacesResponse{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		if _, err := loadEventAPI(tx, scope, in.ApiId); err != nil {
			return err
		}

		all, err := records[api.ChannelNamespace](tx, owned(scope, kindNamespace, *in.ApiId, ""))
		if err != nil {
			return err
		}

		out.ChannelNamespaces, out.NextToken, err = page(all,
			func(n api.ChannelNamespace) string { return *n.Name }, in.NextToken, in.MaxResults)
		return err
	})
	return out, err
}

// eventChannel accepts optional edge slashes and a final subscription wildcard.
func eventChannel(path string, subscribe bool) (channel string, segments []string, err error) {
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/"), "/")
	if len(parts) > 5 {
		return "", nil, badRequest("Channels contain at most five segments.")
	}

	for i, part := range parts {
		if subscribe && part == "*" && i > 0 && i == len(parts)-1 {
			continue
		}

		if !eventSegment.MatchString(part) {
			return "", nil, badRequest("Invalid channel segment.")
		}
	}

	return "/" + strings.Join(parts, "/"), parts, nil
}
