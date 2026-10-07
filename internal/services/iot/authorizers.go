package iot

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindAuthorizer        = "authorizer"
	kindDefaultAuthorizer = "default-authorizer"
	// maxSigningKeys is the most token signing keys an authorizer may have.
	maxSigningKeys = 2
	// maxSessionSeconds bounds an authorizer's disconnectAfterInSeconds.
	maxSessionSeconds = 86400
	// Where clients name an authorizer and sign its token.
	authorizerNameKey = "x-amz-customauthorizer-name"
	authorizerSigKey  = "x-amz-customauthorizer-signature"
)

var (
	authorizerNamePattern = regexp.MustCompile(`^[\w=,@-]{1,128}$`)
	tokenKeyPattern       = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
)

// authorizer is a custom authorizer's record.
type authorizer struct {
	Keys            map[string]string
	Tags            map[string]string
	Name            string
	FunctionARN     string
	TokenKey        string
	Status          string
	Created         float64
	Modified        float64
	SigningDisabled bool
	Caching         bool
}

func authorizerKey(scope request.Scope, name string) string {
	return scope.Key(service, kindAuthorizer, name)
}

func defaultAuthorizerKey(scope request.Scope) string {
	return scope.Key(service, kindDefaultAuthorizer, "name")
}

func loadAuthorizer(tx storage.ReadTx, scope request.Scope, name string) (*authorizer, error) {
	a, found, err := storage.Read[authorizer](tx, authorizerKey(scope, name))
	if err == nil && !found {
		err = notFound("The authorizer %s does not exist.", name)
	}

	return a, err
}

func (a *authorizer) describe(scope request.Scope) *api.AuthorizerDescription {
	d := &api.AuthorizerDescription{
		AuthorizerName: new(a.Name), AuthorizerArn: new(arn(scope, kindAuthorizer, a.Name)),
		AuthorizerFunctionArn: new(a.FunctionARN), Status: new(a.Status), CreationDate: new(a.Created),
		LastModifiedDate: new(a.Modified), SigningDisabled: new(a.SigningDisabled),
		EnableCachingForHttp: new(a.Caching), TokenSigningPublicKeys: a.Keys,
	}
	if a.TokenKey != "" {
		d.TokenKeyName = new(a.TokenKey)
	}

	return d
}

// check validates an authorizer's settings: a Lambda function, and, unless
// signing is disabled, a token key and at most two RSA public keys.
func (a *authorizer) check() error {
	switch {
	case !strings.HasPrefix(a.FunctionARN, "arn:aws:lambda:"):
		return invalid("The authorizer function must be a Lambda function ARN.")
	case a.Status != statusActive && a.Status != statusInactive:
		return invalid("The status must be ACTIVE or INACTIVE.")
	case a.TokenKey != "" && !tokenKeyPattern.MatchString(a.TokenKey):
		return invalid("The token key name must be 1 to 128 letters, digits, underscores, or hyphens.")
	case len(a.Keys) > maxSigningKeys:
		return invalid("An authorizer can have at most %d token signing keys.", maxSigningKeys)
	case !a.SigningDisabled && (a.TokenKey == "" || len(a.Keys) == 0):
		return invalid("An authorizer that signs tokens needs a token key name and token signing public keys.")
	}

	for name, key := range a.Keys {
		if _, err := signingKey(key); err != nil {
			return invalid("The token signing key %s is not an RSA public key in PEM.", name)
		}
	}

	return nil
}

// signingKey parses a PEM RSA public key.
func signingKey(text string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, errors.New("not PEM")
	}

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}

	key, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}

	return key, nil
}

// verifyToken checks a token's base64 RSA SHA-256 signature against the
// authorizer's keys.
func (a *authorizer) verifyToken(token, signature string) bool {
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil || token == "" {
		return false
	}

	digest := sha256.Sum256([]byte(token))
	for _, text := range a.Keys {
		key, err := signingKey(text)
		if err == nil && rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) == nil {
			return true
		}
	}

	return false
}

func (s *Service) CreateAuthorizer(
	ctx context.Context,
	in *api.CreateAuthorizerRequest,
) (*api.CreateAuthorizerResponse, error) {
	name := common.Value(in.AuthorizerName)
	if !authorizerNamePattern.MatchString(name) {
		return nil, invalid("The authorizer name must be 1 to 128 word characters or =,@-.")
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	now := common.Unix(s.now())
	a := &authorizer{
		Name: name, FunctionARN: common.Value(in.AuthorizerFunctionArn), TokenKey: common.Value(in.TokenKeyName),
		Keys: in.TokenSigningPublicKeys, Status: common.Value(common.OrElse(in.Status, new(statusActive))),
		SigningDisabled: common.Value(in.SigningDisabled), Caching: common.Value(in.EnableCachingForHttp),
		Tags: tags, Created: now, Modified: now,
	}
	if checkErr := a.check(); checkErr != nil {
		return nil, checkErr
	}

	var out *api.CreateAuthorizerResponse
	err = s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if exists(tx, authorizerKey(scope, name)) {
			return alreadyExists("The authorizer %s already exists.", name)
		}

		out = &api.CreateAuthorizerResponse{
			AuthorizerName: new(name),
			AuthorizerArn:  new(arn(scope, kindAuthorizer, name)),
		}
		return storage.Write(tx, authorizerKey(scope, name), a)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DescribeAuthorizer(
	ctx context.Context,
	in *api.DescribeAuthorizerRequest,
) (*api.DescribeAuthorizerResponse, error) {
	var out *api.DescribeAuthorizerResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		a, err := loadAuthorizer(tx, scope, common.Value(in.AuthorizerName))
		if err != nil {
			return err
		}

		out = &api.DescribeAuthorizerResponse{AuthorizerDescription: a.describe(scope)}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateAuthorizer(
	ctx context.Context,
	in *api.UpdateAuthorizerRequest,
) (*api.UpdateAuthorizerResponse, error) {
	var out *api.UpdateAuthorizerResponse
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		a, err := loadAuthorizer(tx, scope, common.Value(in.AuthorizerName))
		if err != nil {
			return err
		}

		a.FunctionARN = common.Value(common.OrElse(in.AuthorizerFunctionArn, &a.FunctionARN))
		a.TokenKey = common.Value(common.OrElse(in.TokenKeyName, &a.TokenKey))
		a.Status = common.Value(common.OrElse(in.Status, &a.Status))
		a.Caching = common.Value(common.OrElse(in.EnableCachingForHttp, &a.Caching))
		if in.TokenSigningPublicKeys != nil {
			a.Keys = in.TokenSigningPublicKeys
		}

		if err := a.check(); err != nil {
			return err
		}

		a.Modified = common.Unix(s.now())
		out = &api.UpdateAuthorizerResponse{
			AuthorizerName: new(a.Name), AuthorizerArn: new(arn(scope, kindAuthorizer, a.Name)),
		}
		return storage.Write(tx, authorizerKey(scope, a.Name), a)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteAuthorizer(
	ctx context.Context,
	in *api.DeleteAuthorizerRequest,
) (*api.DeleteAuthorizerResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		a, err := loadAuthorizer(tx, scope, common.Value(in.AuthorizerName))
		if err != nil {
			return err
		}

		if a.Status == statusActive {
			return conflict("The authorizer %s must be INACTIVE before it is deleted.", a.Name)
		}

		if name, _, _ := storage.Read[string](tx, defaultAuthorizerKey(scope)); name != nil && *name == a.Name {
			return conflict("The authorizer %s is the default authorizer.", a.Name)
		}

		tx.Delete(authorizerKey(scope, a.Name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteAuthorizerResponse{}, nil
}

func (s *Service) ListAuthorizers(
	ctx context.Context,
	in *api.ListAuthorizersRequest,
) (*api.ListAuthorizersResponse, error) {
	var out []api.AuthorizerSummary
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		records, err := storage.Records[authorizer](tx, scope.Prefix(service, kindAuthorizer))
		for _, a := range records {
			if in.Status == nil || *in.Status == a.Status {
				out = append(out, api.AuthorizerSummary{
					AuthorizerName: new(a.Name), AuthorizerArn: new(arn(scope, kindAuthorizer, a.Name)),
				})
			}
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	if in.AscendingOrder != nil && !*in.AscendingOrder {
		slices.Reverse(out)
	}

	selected, next, err := page(out, func(a api.AuthorizerSummary) string { return *a.AuthorizerName }, in.Marker,
		in.PageSize)
	if err != nil {
		return nil, err
	}

	if selected == nil {
		selected = []api.AuthorizerSummary{}
	}

	return &api.ListAuthorizersResponse{Authorizers: selected, NextMarker: next}, nil
}

func (s *Service) SetDefaultAuthorizer(
	ctx context.Context,
	in *api.SetDefaultAuthorizerRequest,
) (*api.SetDefaultAuthorizerResponse, error) {
	var out *api.SetDefaultAuthorizerResponse
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		a, err := loadAuthorizer(tx, scope, common.Value(in.AuthorizerName))
		if err != nil {
			return err
		}

		if exists(tx, defaultAuthorizerKey(scope)) {
			return alreadyExists("A default authorizer is already set.")
		}

		out = &api.SetDefaultAuthorizerResponse{
			AuthorizerName: new(a.Name), AuthorizerArn: new(arn(scope, kindAuthorizer, a.Name)),
		}
		return storage.Write(tx, defaultAuthorizerKey(scope), a.Name)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// defaultAuthorizer returns the scope's default authorizer, or nil.
func defaultAuthorizer(tx storage.ReadTx, scope request.Scope) (*authorizer, error) {
	name, found, err := storage.Read[string](tx, defaultAuthorizerKey(scope))
	if err != nil || !found {
		return nil, err
	}

	return loadAuthorizer(tx, scope, *name)
}

func (s *Service) DescribeDefaultAuthorizer(
	ctx context.Context,
	_ *api.DescribeDefaultAuthorizerRequest,
) (*api.DescribeDefaultAuthorizerResponse, error) {
	var out *api.DescribeDefaultAuthorizerResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		a, err := defaultAuthorizer(tx, scope)
		if err == nil && a == nil {
			err = notFound("No default authorizer is set.")
		}

		if err != nil {
			return err
		}

		out = &api.DescribeDefaultAuthorizerResponse{AuthorizerDescription: a.describe(scope)}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ClearDefaultAuthorizer(
	ctx context.Context,
	_ *api.ClearDefaultAuthorizerRequest,
) (*api.ClearDefaultAuthorizerResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if !exists(tx, defaultAuthorizerKey(scope)) {
			return notFound("No default authorizer is set.")
		}

		tx.Delete(defaultAuthorizerKey(scope))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ClearDefaultAuthorizerResponse{}, nil
}

// authRequest is what a client presented to a custom authorizer.
type authRequest struct {
	headers    map[string]string
	mqtt       *api.MqttContext
	token      string
	signature  string
	query      string
	serverName string
}

// authResult is a custom authorizer's decision.
type authResult struct {
	principal     string
	documents     []string
	policies      []*policyDocument
	refreshAfter  int64
	disconnectAt  int64
	authenticated bool
}

func invalidResponse(format string, args ...any) error {
	return &authResponseError{msg: fmt.Sprintf(format, args...)}
}

// authResponseError is an authorizer function's response that IoT cannot
// use.
type authResponseError struct{ msg string }

func (e *authResponseError) Error() string { return e.msg }

// invokeAuthorizer verifies the request's token signature, unless signing is
// disabled, and asks the authorizer's function whether to let it in.
func (s *Service) invokeAuthorizer(ctx context.Context, scope request.Scope, a *authorizer, r *authRequest) (
	*authResult, error,
) {
	verified := false
	if !a.SigningDisabled {
		if !a.verifyToken(r.token, r.signature) {
			return &authResult{}, nil
		}

		verified = true
	}

	protocols := []any{}
	data := map[string]any{}
	if r.serverName != "" || r.mqtt != nil {
		protocols = append(protocols, "tls")
		data["tls"] = map[string]any{"serverName": r.serverName}
	}

	if r.headers != nil {
		protocols = append(protocols, "http")
		data["http"] = map[string]any{"headers": r.headers, "queryString": r.query}
	}

	if m := r.mqtt; m != nil {
		protocols = append(protocols, "mqtt")
		mqtt := map[string]any{"clientId": common.Value(m.ClientId)}
		if m.Username != nil {
			mqtt["username"] = *m.Username
		}

		if m.Password != nil {
			mqtt["password"] = base64.StdEncoding.EncodeToString(m.Password)
		}

		data["mqtt"] = mqtt
	}

	event := map[string]any{
		"token": r.token, "signatureVerified": verified, "protocols": protocols, "protocolData": data,
		"connectionMetadata": map[string]any{"id": uuid.NewV4().String()},
	}
	out, err := s.invokeSync(ctx, scope, a.FunctionARN, event)
	if err != nil {
		return nil, err
	}

	return authDecision(out)
}

// authDecision reads an authorizer function's response.
func authDecision(out any) (*authResult, error) {
	resp, ok := out.(map[string]any)
	if !ok {
		return nil, invalidResponse("The authorizer's response is not a JSON object.")
	}

	authenticated, ok := resp["isAuthenticated"].(bool)
	if !ok {
		return nil, invalidResponse("The authorizer's response needs isAuthenticated.")
	}

	r := &authResult{authenticated: authenticated, policies: []*policyDocument{}}
	if !authenticated {
		return r, nil
	}

	if r.principal, ok = resp["principalId"].(string); !ok || r.principal == "" || len(r.principal) > 128 {
		return nil, invalidResponse("The authorizer's response needs a principalId of 1 to 128 characters.")
	}

	for k, dst := range map[string]*int64{
		"refreshAfterInSeconds":    &r.refreshAfter,
		"disconnectAfterInSeconds": &r.disconnectAt,
	} {
		if v, present := resp[k]; present {
			n, ok := v.(int64)
			if !ok || n < 0 || n > maxSessionSeconds {
				return nil, invalidResponse("%s must be between 0 and %d.", k, maxSessionSeconds)
			}

			*dst = n
		}
	}

	docs, _ := resp["policyDocuments"].([]any)
	for _, d := range docs {
		text, ok := d.(string)
		if !ok {
			b, err := json.Marshal(d)
			if err != nil {
				return nil, invalidResponse("A policy document is not JSON.")
			}

			text = string(b)
		}

		doc, err := parsePolicy(text)
		if err != nil {
			return nil, invalidResponse("A policy document is invalid: %v", err)
		}

		r.documents, r.policies = append(r.documents, text), append(r.policies, doc)
	}

	return r, nil
}

func (s *Service) TestInvokeAuthorizer(
	ctx context.Context,
	in *api.TestInvokeAuthorizerRequest,
) (*api.TestInvokeAuthorizerResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var a *authorizer
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		a, err = loadAuthorizer(tx, scope, common.Value(in.AuthorizerName))
		return err
	})
	if err != nil {
		return nil, err
	}

	r := &authRequest{
		token: common.Value(in.Token), signature: common.Value(in.TokenSignature), mqtt: in.MqttContext,
	}
	if h := in.HttpContext; h != nil {
		r.headers, r.query = h.Headers, common.Value(h.QueryString)
		if r.headers == nil {
			r.headers = map[string]string{}
		}
	}

	if in.TlsContext != nil {
		r.serverName = common.Value(in.TlsContext.ServerName)
	}

	result, err := s.invokeAuthorizer(ctx, scope, a, r)
	var bad *authResponseError
	switch {
	case errors.As(err, &bad):
		return nil, awserror.New("InvalidResponseException", bad.msg)
	case err != nil:
		return nil, err
	}

	out := &api.TestInvokeAuthorizerResponse{
		IsAuthenticated: new(result.authenticated), PolicyDocuments: result.documents,
	}
	if result.authenticated {
		out.PrincipalId = new(result.principal)
		out.RefreshAfterInSeconds, out.DisconnectAfterInSeconds = new(result.refreshAfter), new(result.disconnectAt)
	}

	if out.PolicyDocuments == nil {
		out.PolicyDocuments = []string{}
	}

	return out, nil
}
