package appsync

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/appsync"
	lambdaapi "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/common"
)

// errUnauthorized is AppSync's answer to a request no authorization mode of
// the API accepts.
var errUnauthorized = &fieldError{Type: "UnauthorizedException", Message: "You are not authorized to make this call."}

// directiveModes maps AppSync's authorization directives to the modes they
// allow.
var directiveModes = map[string]string{
	"aws_api_key": authAPIKey, "aws_iam": authIAM, "aws_oidc": authOIDC, "aws_lambda": authLambda,
	"aws_cognito_user_pools": authCognito, "aws_auth": authCognito,
}

// caller is how a request was authorized and the identity its resolvers see.
type caller struct {
	identity any
	mode     string
	groups   []string
	// denied are the fields a Lambda authorizer denied, as Type.field.
	denied []string
	// cognitoDeny is set for a primary Cognito mode whose defaultAction is
	// DENY, which allows only fields that name the caller's groups.
	cognitoDeny bool
}

// provider is one authorization mode of an API.
type provider struct {
	pool       *api.CognitoUserPoolConfig
	oidc       *api.OpenIDConnectConfig
	authorizer *api.LambdaAuthorizerConfig
	kind       string
	deny       bool
}

func providers(a *api.GraphqlApi) []provider {
	out := []provider{{
		kind: *a.AuthenticationType, oidc: a.OpenIDConnectConfig, authorizer: a.LambdaAuthorizerConfig,
	}}
	if p := a.UserPoolConfig; p != nil {
		out[0].pool = &api.CognitoUserPoolConfig{
			UserPoolId: p.UserPoolId, AwsRegion: p.AwsRegion, AppIdClientRegex: p.AppIdClientRegex,
		}
		out[0].deny = common.Value(p.DefaultAction) == "DENY"
	}

	for _, p := range a.AdditionalAuthenticationProviders {
		out = append(out, provider{
			kind: *p.AuthenticationType, pool: p.UserPoolConfig, oidc: p.OpenIDConnectConfig,
			authorizer: p.LambdaAuthorizerConfig,
		})
	}

	return out
}

// authorize finds the mode that accepts the request: an API key it sends,
// its SigV4 signature, or the token in its Authorization header.
func (s *Service) authorize(ctx context.Context, g *graph, r *execute.Request, q *query) (*caller, error) {
	all := providers(g.api)
	has := func(kind string) bool {
		return slices.ContainsFunc(all, func(p provider) bool { return p.kind == kind })
	}
	sourceIP := []string{r.SourceIP}
	switch {
	case r.Header.Get("X-Api-Key") != "" && has(authAPIKey):
		if !g.keyValid {
			return nil, errUnauthorized
		}

		return &caller{mode: authAPIKey}, nil
	case r.Caller != nil && has(authIAM):
		return &caller{mode: authIAM, identity: map[string]any{
			"accountId": r.Caller.AccountID, "username": r.Caller.AccessKey, "sourceIp": sourceIP,
			"userArn": "arn:aws:iam::" + r.Caller.AccountID + ":root", "cognitoIdentityPoolId": nil,
			"cognitoIdentityId": nil, "cognitoIdentityAuthType": nil, "cognitoIdentityAuthProvider": nil,
		}}, nil
	}

	token := r.Header.Get("Authorization")
	if token == "" {
		return nil, errUnauthorized
	}

	for _, p := range all {
		c, err := s.tokenCaller(ctx, g, p, token, r, q)
		if !errors.Is(err, errUnauthorized) {
			return c, err
		}
	}

	return nil, errUnauthorized
}

// tokenCaller authorizes token with a Cognito, OIDC, or Lambda provider,
// failing with errUnauthorized when p does not accept it.
func (s *Service) tokenCaller(
	ctx context.Context,
	g *graph,
	p provider,
	token string,
	r *execute.Request,
	q *query,
) (*caller, error) {
	switch p.kind {
	case authCognito:
		if s.pools == nil {
			return nil, errUnauthorized
		}

		claims, err := s.pools.Verify(ctx, common.Value(p.pool.UserPoolId), token)
		if err != nil || !clientMatches(claims, p.pool.AppIdClientRegex) {
			return nil, errors.Join(errUnauthorized, err)
		}

		return tokenIdentity(authCognito, claims, r.SourceIP, p.deny), nil
	case authOIDC:
		var audiences []string
		if id := common.Value(p.oidc.ClientId); id != "" {
			audiences = []string{id}
		}

		claims, err := s.verifier.Verify(ctx, token, common.Value(p.oidc.Issuer), audiences)
		if err != nil {
			return nil, errors.Join(errUnauthorized, err)
		}

		return tokenIdentity(authOIDC, claims, r.SourceIP, false), nil
	case authLambda:
		return s.lambdaCaller(ctx, g, p.authorizer, token, r, q)
	}

	return nil, errUnauthorized
}

// clientMatches reports whether a token's app client matches a Cognito
// configuration's appIdClientRegex, if it has one.
func clientMatches(claims map[string]any, pattern *string) bool {
	if pattern == nil {
		return true
	}

	re, err := regexp.Compile(*pattern)
	if err != nil {
		return false
	}

	client, _ := claims["aud"].(string)
	if client == "" {
		client, _ = claims["client_id"].(string)
	}

	return re.MatchString(client)
}

func tokenIdentity(mode string, claims map[string]any, sourceIP string, deny bool) *caller {
	var groups []string
	if list, ok := claims["cognito:groups"].([]any); ok {
		for _, g := range list {
			if name, isString := g.(string); isString {
				groups = append(groups, name)
			}
		}
	}

	username, _ := claims["cognito:username"].(string)
	if username == "" {
		username, _ = claims["username"].(string)
	}

	identity := map[string]any{
		"sub": claims["sub"], "issuer": claims["iss"], "claims": claims, "sourceIp": []string{sourceIP},
	}
	if mode == authCognito {
		identity["username"], identity["groups"] = username, groups
		identity["defaultAuthStrategy"] = map[bool]string{true: "DENY", false: "ALLOW"}[deny]
	}

	return &caller{mode: mode, identity: identity, groups: groups, cognitoDeny: deny}
}

// lambdaCaller asks a Lambda authorizer whether token may make the request.
func (s *Service) lambdaCaller(
	ctx context.Context,
	g *graph,
	config *api.LambdaAuthorizerConfig,
	token string,
	r *execute.Request,
	q *query,
) (*caller, error) {
	if pattern := common.Value(config.IdentityValidationExpression); pattern != "" {
		if re, err := regexp.Compile(pattern); err != nil || !re.MatchString(token) {
			return nil, errors.Join(errUnauthorized, err)
		}
	}

	payload, err := json.Marshal(map[string]any{
		"authorizationToken": token,
		"requestContext": map[string]any{
			"apiId": *g.api.ApiId, "accountId": common.Value(g.api.Owner), "requestId": common.RandomID(32),
			"queryString": q.Query, "operationName": q.OperationName, "variables": q.Variables,
		},
		"requestHeaders": headers(r),
	})
	if err != nil {
		return nil, err
	}

	out, err := s.functions.Invoke(ctx, &lambdaapi.InvocationRequest{
		FunctionName: config.AuthorizerUri, Payload: payload,
	})
	if err != nil {
		return nil, err
	}

	var answer struct {
		ResolverContext map[string]any `json:"resolverContext"`
		DeniedFields    []string       `json:"deniedFields"`
		IsAuthorized    bool           `json:"isAuthorized"`
	}
	if out.FunctionError != nil || json.Unmarshal(out.Payload, &answer) != nil || !answer.IsAuthorized {
		return nil, errUnauthorized
	}

	c := &caller{mode: authLambda, identity: map[string]any{"resolverContext": answer.ResolverContext}}
	for _, f := range answer.DeniedFields {
		// A field is denied by its ARN (…/types/<type>/fields/<field>) or as
		// <type>.<field>.
		if m := deniedARN.FindStringSubmatch(f); m != nil {
			f = m[1] + "." + m[2]
		}

		c.denied = append(c.denied, f)
	}

	return c, nil
}

var deniedARN = regexp.MustCompile(`/types/([^/]+)/fields/([^/]+)$`)

// allowed reports whether c may read field f of parent, a type of an API
// whose primary mode is primary: the field's authorization directives, or else
// its type's, name the modes that may, and with neither only the primary mode
// may.
func (c *caller) allowed(primary string) func(parent *ast.Definition, f *ast.FieldDefinition) bool {
	return func(parent *ast.Definition, f *ast.FieldDefinition) bool {
		if slices.Contains(c.denied, parent.Name+"."+f.Name) {
			return false
		}

		directives := f.Directives
		if !slices.ContainsFunc(directives, isAuth) {
			directives = parent.Directives
		}

		if !slices.ContainsFunc(directives, isAuth) {
			return c.mode == primary && !c.cognitoDeny
		}

		for _, d := range directives {
			if directiveModes[d.Name] != c.mode {
				continue
			}

			groups := d.Arguments.ForName("cognito_groups")
			if groups == nil {
				return !c.cognitoDeny
			}

			for _, g := range groups.Value.Children {
				if slices.Contains(c.groups, g.Value.Raw) {
					return true
				}
			}
		}

		return false
	}
}

func isAuth(d *ast.Directive) bool { return directiveModes[d.Name] != "" }

// headers are a request's headers as resolvers see them: lowercase names,
// each with its first value.
func headers(r *execute.Request) map[string]string {
	out := map[string]string{}
	for k, v := range r.Header {
		if len(v) > 0 {
			out[strings.ToLower(k)] = v[0]
		}
	}

	return out
}
