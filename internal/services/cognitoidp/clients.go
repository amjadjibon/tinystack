package cognitoidp

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxClients  = 1000
	maxPage     = 60
	unitMinutes = "minutes"
	unitHours   = "hours"
	unitDays    = "days"

	tokenAccess  = "AccessToken"
	tokenID      = "IdToken"
	tokenRefresh = "RefreshToken"

	flowAdminPassword = "ALLOW_ADMIN_USER_PASSWORD_AUTH"
	flowUserPassword  = "ALLOW_USER_PASSWORD_AUTH"
	flowRefresh       = "ALLOW_REFRESH_TOKEN_AUTH"
	flowSRP           = "ALLOW_USER_SRP_AUTH"
	flowCustom        = "ALLOW_CUSTOM_AUTH"

	// flowClientCredentials is the OAuth client credentials grant.
	flowClientCredentials = "client_credentials"

	legacyUserPassword  = "USER_PASSWORD_AUTH"
	legacyAdminPassword = "ADMIN_NO_SRP_AUTH" // #nosec G101 -- an auth flow name, not a credential.
)

// client is an app client record.
type client struct {
	Created  time.Time
	Modified time.Time
	Pool     string
	ID       string
	Name     string
	Secret   string `json:",omitempty"`
	Settings clientSettings
}

// clientSettings keep the wire names, like pool settings; UpdateUserPoolClient
// replaces them all.
type clientSettings struct {
	TokenValidityUnits                       *api.TokenValidityUnitsType `json:",omitempty"`
	RefreshTokenValidity                     *int64                      `json:",omitempty"`
	AccessTokenValidity                      *int64                      `json:",omitempty"`
	IdTokenValidity                          *int64                      `json:",omitempty"`
	AuthSessionValidity                      *int64                      `json:",omitempty"`
	DefaultRedirectURI                       *string                     `json:",omitempty"`
	PreventUserExistenceErrors               *string                     `json:",omitempty"`
	AllowedOAuthFlowsUserPoolClient          *bool                       `json:",omitempty"`
	EnableTokenRevocation                    *bool                       `json:",omitempty"`
	EnablePropagateAdditionalUserContextData *bool                       `json:",omitempty"`
	ReadAttributes                           []string                    `json:",omitempty"`
	WriteAttributes                          []string                    `json:",omitempty"`
	ExplicitAuthFlows                        []string                    `json:",omitempty"`
	SupportedIdentityProviders               []string                    `json:",omitempty"`
	CallbackURLs                             []string                    `json:",omitempty"`
	LogoutURLs                               []string                    `json:",omitempty"`
	AllowedOAuthFlows                        []string                    `json:",omitempty"`
	AllowedOAuthScopes                       []string                    `json:",omitempty"`
}

// clientIndex locates a client from its ID alone.
type clientIndex struct {
	Account string
	Region  string
	Pool    string
}

var (
	unitDuration = map[string]time.Duration{
		"seconds": time.Second, unitMinutes: time.Minute, unitHours: time.Hour, unitDays: 24 * time.Hour,
	}
	legacyFlows  = []string{legacyAdminPassword, "CUSTOM_AUTH_FLOW_ONLY", legacyUserPassword}
	allowFlows   = []string{flowAdminPassword, flowCustom, flowUserPassword, flowSRP, flowRefresh, "ALLOW_USER_AUTH"}
	oauthScopes  = []string{"phone", attrEmail, "openid", "profile", adminScope}
	defaultFlows = []string{flowRefresh, flowSRP, flowCustom}
)

// validity returns how long a client's tokens of kind (tokenAccess, tokenID,
// or tokenRefresh) last.
func (c *clientSettings) validity(kind string) time.Duration {
	units := map[string]*string{}
	if u := c.TokenValidityUnits; u != nil {
		units = map[string]*string{tokenAccess: u.AccessToken, tokenID: u.IdToken, tokenRefresh: u.RefreshToken}
	}

	amount := map[string]*int64{
		tokenAccess: c.AccessTokenValidity, tokenID: c.IdTokenValidity, tokenRefresh: c.RefreshTokenValidity,
	}[kind]
	unit := common.Value(units[kind])
	switch {
	case unit == "" && kind == tokenRefresh:
		unit = unitDays
	case unit == "":
		unit = unitHours
	}

	if amount == nil {
		if kind == tokenRefresh {
			return 30 * 24 * time.Hour
		}

		return time.Hour
	}

	return time.Duration(*amount) * unitDuration[unit]
}

// flows returns the auth flows a client allows, mapping the legacy names.
func (c *clientSettings) flows() []string {
	if len(c.ExplicitAuthFlows) == 0 {
		return defaultFlows
	}

	out := slices.Clone(c.ExplicitAuthFlows)
	for _, flow := range c.ExplicitAuthFlows {
		switch flow {
		case legacyAdminPassword:
			out = append(out, flowAdminPassword, flowRefresh, flowSRP)
		case legacyUserPassword:
			out = append(out, flowUserPassword, flowRefresh, flowSRP)
		case "CUSTOM_AUTH_FLOW_ONLY":
			out = append(out, flowCustom, flowRefresh)
		}
	}

	return out
}

// checkClient validates client settings against the pool, whose resource
// servers define the custom scopes, and fills defaults.
func checkClient(c *clientSettings, p *pool, custom []string, secret bool) error {
	if err := checkValidity(c); err != nil {
		return err
	}

	legacy, allow := false, false
	for _, flow := range c.ExplicitAuthFlows {
		legacy, allow = legacy || slices.Contains(legacyFlows, flow), allow || slices.Contains(allowFlows, flow)
		if !slices.Contains(legacyFlows, flow) && !slices.Contains(allowFlows, flow) {
			return invalidParameter("Invalid explicit auth flow %s.", flow)
		}
	}

	if legacy && allow {
		return invalidParameter("ExplicitAuthFlows can't have both ALLOW_ and legacy values.")
	}

	for _, provider := range c.SupportedIdentityProviders {
		if provider != "COGNITO" {
			return invalidParameter("The provider %s does not exist for User Pool %s", provider, p.ID)
		}
	}

	for _, attrs := range [][]string{c.ReadAttributes, c.WriteAttributes} {
		for _, attr := range attrs {
			if p.attribute(attr) == nil {
				return invalidParameter("Invalid read or write attribute %s.", attr)
			}
		}
	}

	if slices.Contains(c.WriteAttributes, attrSub) {
		return invalidParameter("The sub attribute cannot be writable.")
	}

	if err := checkOAuth(c, custom, secret); err != nil {
		return err
	}

	if !oneOf(c.PreventUserExistenceErrors, "ENABLED", "LEGACY") {
		return invalidParameter("PreventUserExistenceErrors must be ENABLED or LEGACY.")
	}

	if c.RefreshTokenValidity == nil {
		c.RefreshTokenValidity = new(int64(30))
	}

	if c.AuthSessionValidity == nil {
		c.AuthSessionValidity = new(int64(3))
	}

	if c.EnableTokenRevocation == nil {
		c.EnableTokenRevocation = new(true)
	}

	if c.AllowedOAuthFlowsUserPoolClient == nil {
		c.AllowedOAuthFlowsUserPoolClient = new(false)
	}

	if c.EnablePropagateAdditionalUserContextData == nil {
		c.EnablePropagateAdditionalUserContextData = new(false)
	}

	return nil
}

func checkValidity(c *clientSettings) error {
	if u := c.TokenValidityUnits; u != nil {
		for _, unit := range []*string{u.AccessToken, u.IdToken, u.RefreshToken} {
			if unit != nil && unitDuration[*unit] == 0 {
				return invalidParameter("Invalid token validity unit %s.", *unit)
			}
		}
	}

	for kind, amount := range map[string]*int64{
		tokenAccess: c.AccessTokenValidity, tokenID: c.IdTokenValidity, tokenRefresh: c.RefreshTokenValidity,
	} {
		if amount == nil {
			continue
		}

		d, lowest, highest := c.validity(kind), 5*time.Minute, 24*time.Hour
		if kind == tokenRefresh {
			lowest, highest = time.Hour, 3650*24*time.Hour
		}

		if *amount <= 0 || d < lowest || d > highest {
			return invalidParameter("%sValidity must be between %v and %v.", kind, lowest, highest)
		}
	}

	if v := c.AuthSessionValidity; v != nil && (*v < 3 || *v > 15) {
		return invalidParameter("AuthSessionValidity must be between 3 and 15 minutes.")
	}

	return nil
}

func checkOAuth(c *clientSettings, custom []string, secret bool) error {
	for _, flow := range c.AllowedOAuthFlows {
		if flow != "code" && flow != "implicit" && flow != flowClientCredentials {
			return invalidParameter("Invalid OAuth flow %s.", flow)
		}
	}

	for _, scope := range c.AllowedOAuthScopes {
		if !slices.Contains(oauthScopes, scope) && !slices.Contains(custom, scope) {
			return awserror.New("ScopeDoesNotExistException", "Invalid scope requested: "+scope)
		}
	}

	if slices.Contains(c.AllowedOAuthFlows, flowClientCredentials) {
		switch {
		case !secret:
			return awserror.New("InvalidOAuthFlowException",
				"client_credentials flow can not be selected if client does not have a client secret.")
		case len(c.AllowedOAuthFlows) > 1:
			return awserror.New("InvalidOAuthFlowException",
				"client_credentials flow can not be selected along with code flow or implicit flow.")
		}

		for _, scope := range c.AllowedOAuthScopes {
			if !slices.Contains(custom, scope) {
				return awserror.New("InvalidOAuthFlowException",
					"client_credentials flow can only be used with custom scopes, not "+scope+".")
			}
		}
	}

	if common.Value(c.AllowedOAuthFlowsUserPoolClient) &&
		(len(c.AllowedOAuthFlows) == 0 || len(c.AllowedOAuthScopes) == 0) {
		return awserror.New("InvalidOAuthFlowException", "AllowedOAuthFlows and AllowedOAuthScopes are required "+
			"when AllowedOAuthFlowsUserPoolClient is true.")
	}

	if len(c.CallbackURLs) > 100 || len(c.LogoutURLs) > 100 {
		return invalidParameter("A client can have at most 100 callback and 100 logout URLs.")
	}

	for _, u := range append(slices.Clone(c.CallbackURLs), c.LogoutURLs...) {
		if !strings.Contains(u, ":") {
			return invalidParameter("Invalid URL %s.", u)
		}
	}

	if c.DefaultRedirectURI != nil && !slices.Contains(c.CallbackURLs, *c.DefaultRedirectURI) {
		return invalidParameter("DefaultRedirectURI must be one of the CallbackURLs.")
	}

	return nil
}

func (c *client) output() (*api.UserPoolClientType, error) {
	out, err := convert[api.UserPoolClientType](c.Settings)
	if err != nil {
		return nil, err
	}

	out.UserPoolId, out.ClientId, out.ClientName = new(c.Pool), new(c.ID), new(c.Name)
	out.CreationDate, out.LastModifiedDate = common.SecondsMilli(c.Created), common.SecondsMilli(c.Modified)
	if c.Secret != "" {
		out.ClientSecret = new(c.Secret)
	}

	return &out, nil
}

// loadClient returns the pool's client id names.
func loadClient(tx storage.ReadTx, scope request.Scope, p *pool, id *string) (*client, error) {
	if id == nil || *id == "" {
		return nil, invalidParameter("ClientId is required.")
	}

	c, ok, err := storage.Read[client](tx, clientKey(scope, p.ID, *id))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, notFound("User pool client %s does not exist.", *id)
	}

	return c, nil
}

func (s *Service) CreateUserPoolClient(
	ctx context.Context,
	in *api.CreateUserPoolClientRequest,
) (*api.CreateUserPoolClientResponse, error) {
	if err := checkName(in.ClientName, "ClientName"); err != nil {
		return nil, err
	}

	st, err := convert[clientSettings](in)
	if err != nil {
		return nil, err
	}

	var c *client
	err = s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		custom, scopesErr := customScopes(tx, scope, p.ID)
		if scopesErr != nil {
			return scopesErr
		}

		if settingsErr := checkClient(&st, p, custom, common.Value(in.GenerateSecret)); settingsErr != nil {
			return settingsErr
		}

		if len(tx.Keys(prefix(scope, "client")+p.ID+"/")) >= maxClients {
			return awserror.New("LimitExceededException", "The user pool has reached its limit of app clients.")
		}

		now := s.now().UTC()
		c = &client{
			Pool: p.ID, ID: randomString(lower, 26), Name: *in.ClientName, Created: now, Modified: now,
			Settings: st,
		}
		if common.Value(in.GenerateSecret) {
			c.Secret = randomString(lower, 51)
		}

		index := clientIndex{Account: scope.AccountID, Region: scope.Region, Pool: p.ID}
		if writeErr := storage.Write(tx, clientIndexKey(c.ID), index); writeErr != nil {
			return writeErr
		}

		return storage.Write(tx, clientKey(scope, p.ID, c.ID), c)
	})
	if err != nil {
		return nil, err
	}

	out, err := c.output()
	if err != nil {
		return nil, err
	}

	return &api.CreateUserPoolClientResponse{UserPoolClient: out}, nil
}

func (s *Service) DescribeUserPoolClient(
	ctx context.Context,
	in *api.DescribeUserPoolClientRequest,
) (*api.DescribeUserPoolClientResponse, error) {
	var out *api.UserPoolClientType
	err := s.view(ctx, in.UserPoolId, func(tx storage.ReadTx, scope request.Scope, p *pool) error {
		c, loadErr := loadClient(tx, scope, p, in.ClientId)
		if loadErr != nil {
			return loadErr
		}

		var outErr error
		out, outErr = c.output()
		return outErr
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeUserPoolClientResponse{UserPoolClient: out}, nil
}

func (s *Service) ListUserPoolClients(
	ctx context.Context,
	in *api.ListUserPoolClientsRequest,
) (*api.ListUserPoolClientsResponse, error) {
	n, err := limit(in.MaxResults, "MaxResults")
	if err != nil {
		return nil, err
	}

	var clients []*client
	err = s.view(ctx, in.UserPoolId, func(tx storage.ReadTx, scope request.Scope, p *pool) error {
		for _, id := range ids(tx, prefix(scope, "client")+p.ID+"/") {
			c, _, readErr := storage.Read[client](tx, clientKey(scope, p.ID, id))
			if readErr != nil {
				return readErr
			}

			clients = append(clients, c)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(clients, func(c *client) string { return c.ID }, in.NextToken, n)
	if err != nil {
		return nil, err
	}

	out := &api.ListUserPoolClientsResponse{UserPoolClients: []api.UserPoolClientDescription{}, NextToken: next}
	for _, c := range selected {
		out.UserPoolClients = append(out.UserPoolClients, api.UserPoolClientDescription{
			ClientId: new(c.ID), UserPoolId: new(c.Pool), ClientName: new(c.Name),
		})
	}

	return out, nil
}

func (s *Service) UpdateUserPoolClient(
	ctx context.Context,
	in *api.UpdateUserPoolClientRequest,
) (*api.UpdateUserPoolClientResponse, error) {
	if in.ClientName != nil {
		if err := checkName(in.ClientName, "ClientName"); err != nil {
			return nil, err
		}
	}

	st, err := convert[clientSettings](in)
	if err != nil {
		return nil, err
	}

	var c *client
	err = s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		var loadErr error
		if c, loadErr = loadClient(tx, scope, p, in.ClientId); loadErr != nil {
			return loadErr
		}

		custom, scopesErr := customScopes(tx, scope, p.ID)
		if scopesErr != nil {
			return scopesErr
		}

		if settingsErr := checkClient(&st, p, custom, c.Secret != ""); settingsErr != nil {
			return settingsErr
		}

		c.Settings, c.Modified = st, s.now().UTC()
		if in.ClientName != nil {
			c.Name = *in.ClientName
		}

		return storage.Write(tx, clientKey(scope, p.ID, c.ID), c)
	})
	if err != nil {
		return nil, err
	}

	out, err := c.output()
	if err != nil {
		return nil, err
	}

	return &api.UpdateUserPoolClientResponse{UserPoolClient: out}, nil
}

func (s *Service) DeleteUserPoolClient(
	ctx context.Context,
	in *api.DeleteUserPoolClientRequest,
) (*api.DeleteUserPoolClientOutput, error) {
	err := s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		c, loadErr := loadClient(tx, scope, p, in.ClientId)
		if loadErr != nil {
			return loadErr
		}

		tx.Delete(clientKey(scope, p.ID, c.ID))
		tx.Delete(clientIndexKey(c.ID))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteUserPoolClientOutput{}, nil
}
