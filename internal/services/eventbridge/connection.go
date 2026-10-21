package eventbridge

import (
	"context"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// A connection holds the authorization of API destinations. It is
// "<account>/<region>/eventbridge/connection/<name>", with its secrets in the
// record: Describe never returns them. AWS keeps them in a Secrets Manager
// secret, "events!connection/<name>/<id>", whose ARN a connection reports as
// SecretArn; here that secret does not exist.

const (
	connectionAuthorized   = "AUTHORIZED"
	connectionDeauthorized = "DEAUTHORIZED"
	authAPIKey             = "API_KEY"
	authBasic              = "BASIC"
	authOAuth              = "OAUTH_CLIENT_CREDENTIALS"
	maxAuthText            = 512
	maxEndpoint            = 2048
)

// resourceName is the name of a connection or API destination.
var resourceName = regexp.MustCompile(`^[.\-_A-Za-z0-9]{1,64}$`)

// httpParameter is a header, query string, or body parameter of a connection. A secret one is never described.
type httpParameter struct {
	Key    string
	Value  string
	Secret bool `json:",omitempty"`
}

// httpParameters are the parameters a connection adds to every request it authorizes.
type httpParameters struct {
	Headers []httpParameter `json:",omitempty"`
	Query   []httpParameter `json:",omitempty"`
	Body    []httpParameter `json:",omitempty"`
}

// connection is a connection's record, with the members of its authorization type set. Times are Unix
// milliseconds.
//
//nolint:govet // The field order groups the members by authorization type.
type connection struct {
	Name        string
	ID          string
	Description string `json:",omitempty"`
	Type        string
	State       string
	Created     int64
	Modified    int64
	Authorized  int64
	// KeyName and KeyValue are an API key.
	KeyName  string `json:",omitempty"`
	KeyValue string `json:",omitempty"`
	// Username and Password are Basic credentials.
	Username string `json:",omitempty"`
	Password string `json:",omitempty"`
	// ClientID to OAuthParameters are an OAuth client credentials grant: the endpoint that issues tokens, the
	// method it takes, and its extra parameters.
	ClientID        string `json:",omitempty"`
	ClientSecret    string `json:",omitempty"`
	Endpoint        string `json:",omitempty"`
	Method          string `json:",omitempty"`
	OAuthParameters httpParameters
	// Parameters are the InvocationHttpParameters.
	Parameters httpParameters
}

func connectionPrefix(scope request.Scope) string {
	return scope.AccountID + "/" + scope.Region + "/eventbridge/connection/"
}

// resourceARNPrefix is the start of the ARNs of a kind of connection or API destination in scope.
func resourceARNPrefix(scope request.Scope, kind string) string {
	return "arn:aws:events:" + scope.Region + ":" + scope.AccountID + ":" + kind + "/"
}

func connectionARN(scope request.Scope, name, id string) string {
	return resourceARNPrefix(scope, "connection") + name + "/" + id
}

func connectionNotFound(name string) error {
	return awserror.New("ResourceNotFoundException", "Failed to find connection with name "+name+".")
}

func (c *connection) arn(scope request.Scope) string { return connectionARN(scope, c.Name, c.ID) }

// secretARN is the ARN AWS gives the secret that holds the connection's credentials.
func (c *connection) secretARN(scope request.Scope) string {
	return "arn:aws:secretsmanager:" + scope.Region + ":" + scope.AccountID + ":secret:events!connection/" + c.Name +
		"/" + c.ID + "-" + strings.ReplaceAll(c.ID, "-", "")[:6]
}

// loadConnection reads the connection a request names.
func loadConnection(tx storage.ReadTx, scope request.Scope, name string) (*connection, error) {
	if !resourceName.MatchString(name) {
		return nil, invalid("Connection name must be 1 to 64 characters from [.-_A-Za-z0-9].")
	}

	c, ok, err := storage.Read[connection](tx, connectionPrefix(scope)+name)
	if err == nil && !ok {
		return nil, connectionNotFound(name)
	}

	return c, err
}

// connectionByARN reads the connection an ARN names, which must be in scope and still carry the ARN's ID.
func connectionByARN(tx storage.ReadTx, scope request.Scope, arn string) (*connection, error) {
	rest, ok := strings.CutPrefix(arn, resourceARNPrefix(scope, "connection"))
	name, id, found := strings.Cut(rest, "/")
	if !ok || !found {
		return nil, awserror.New("ResourceNotFoundException", "Failed to find connection with arn "+arn+".")
	}

	c, err := loadConnection(tx, scope, name)
	if err == nil && c.ID != id {
		return nil, awserror.New("ResourceNotFoundException", "Failed to find connection with arn "+arn+".")
	}

	return c, err
}

// checkText checks a name, value, or URL a connection takes.
func checkText(what, v string, limit int) error {
	control := func(r rune) bool { return r < ' ' && r != '\t' || r == 0x7f }
	if v == "" || len(v) > limit || strings.ContainsFunc(v, control) {
		return invalid("%s must be 1 to %d characters without control characters.", what, limit)
	}

	return nil
}

// checkEndpoint checks the URL of an API destination or an OAuth authorization endpoint.
func checkEndpoint(what, v string) error {
	if err := checkText(what, v, maxEndpoint); err != nil {
		return err
	}

	u, err := url.Parse(v)
	if err != nil || u.Host == "" || u.Scheme != "https" && u.Scheme != "http" {
		return invalid("%s must be an http or https URL.", what)
	}

	return nil
}

// authInput is the AuthParameters of a request to create or update a connection. Its blocks are nil where the
// request leaves them out.
type authInput struct {
	keyName, keyValue                        *string
	username, password                       *string
	clientID, clientSecret, endpoint, method *string
	oauthParameters, parameters              *api.ConnectionHttpParameters
	hasAPIKey, hasBasic, hasOAuth            bool
}

func createAuth(in *api.CreateConnectionAuthRequestParameters) authInput {
	out := authInput{parameters: in.InvocationHttpParameters}
	if p := in.ApiKeyAuthParameters; p != nil {
		out.hasAPIKey, out.keyName, out.keyValue = true, p.ApiKeyName, p.ApiKeyValue
	}

	if p := in.BasicAuthParameters; p != nil {
		out.hasBasic, out.username, out.password = true, p.Username, p.Password
	}

	if p := in.OAuthParameters; p != nil {
		out.hasOAuth, out.endpoint, out.method, out.oauthParameters = true, p.AuthorizationEndpoint, p.HttpMethod,
			p.OAuthHttpParameters
		if client := p.ClientParameters; client != nil {
			out.clientID, out.clientSecret = client.ClientID, client.ClientSecret
		}
	}

	return out
}

func updateAuth(in *api.UpdateConnectionAuthRequestParameters) authInput {
	out := authInput{parameters: in.InvocationHttpParameters}
	if p := in.ApiKeyAuthParameters; p != nil {
		out.hasAPIKey, out.keyName, out.keyValue = true, p.ApiKeyName, p.ApiKeyValue
	}

	if p := in.BasicAuthParameters; p != nil {
		out.hasBasic, out.username, out.password = true, p.Username, p.Password
	}

	if p := in.OAuthParameters; p != nil {
		out.hasOAuth, out.endpoint, out.method, out.oauthParameters = true, p.AuthorizationEndpoint, p.HttpMethod,
			p.OAuthHttpParameters
		if client := p.ClientParameters; client != nil {
			out.clientID, out.clientSecret = client.ClientID, client.ClientSecret
		}
	}

	return out
}

// set replaces *dst with *v, where the request gives one, after checking it.
func set(dst *string, what string, v *string, limit int) error {
	if v == nil {
		return nil
	}

	if err := checkText(what, *v, limit); err != nil {
		return err
	}

	*dst = *v
	return nil
}

// apply makes c a connection of the given authorization type with in's members. Members in leaves out keep their
// value, except when the type changes, so an update need not repeat what it keeps; c must be complete after it.
func (c *connection) apply(kind string, in *authInput) error {
	if !slices.Contains([]string{authAPIKey, authBasic, authOAuth}, kind) {
		return invalid("AuthorizationType must be %s, %s, or %s.", authAPIKey, authBasic, authOAuth)
	}

	own := map[string]bool{authAPIKey: in.hasAPIKey, authBasic: in.hasBasic, authOAuth: in.hasOAuth}
	for other, given := range own {
		if given && other != kind {
			return invalid(
				"AuthParameters hold %s parameters, which do not match the AuthorizationType %s.",
				other,
				kind,
			)
		}
	}

	if kind != c.Type {
		*c = connection{
			Name: c.Name, ID: c.ID, Description: c.Description, State: c.State, Created: c.Created,
			Modified: c.Modified, Authorized: c.Authorized, Type: kind, Parameters: c.Parameters,
		}
	}

	for _, field := range []struct {
		dst, value *string
		what       string
	}{
		{&c.KeyName, in.keyName, "ApiKeyName"},
		{&c.KeyValue, in.keyValue, "ApiKeyValue"},
		{&c.Username, in.username, "Username"},
		{&c.Password, in.password, "Password"},
		{&c.ClientID, in.clientID, "ClientID"},
		{&c.ClientSecret, in.clientSecret, "ClientSecret"},
	} {
		if err := set(field.dst, field.what, field.value, maxAuthText); err != nil {
			return err
		}
	}

	if in.keyName != nil && !headerName.MatchString(c.KeyName) {
		return invalid("ApiKeyName must be a valid HTTP header name.")
	}

	if in.endpoint != nil {
		if err := checkEndpoint("AuthorizationEndpoint", *in.endpoint); err != nil {
			return err
		}

		c.Endpoint = *in.endpoint
	}

	if in.method != nil {
		if !slices.Contains([]string{"GET", "POST", "PUT"}, *in.method) {
			return invalid("HttpMethod of OAuthParameters must be GET, POST, or PUT.")
		}

		c.Method = *in.method
	}

	var err error
	if in.oauthParameters != nil {
		if c.OAuthParameters, err = newParameters(in.oauthParameters, &c.OAuthParameters); err != nil {
			return err
		}
	}

	if in.parameters != nil {
		if c.Parameters, err = newParameters(in.parameters, &c.Parameters); err != nil {
			return err
		}
	}

	return c.complete()
}

// complete checks that the connection has everything its authorization type needs.
func (c *connection) complete() error {
	var missing []string
	switch c.Type {
	case authAPIKey:
		missing = missingMembers(map[string]string{"ApiKeyName": c.KeyName, "ApiKeyValue": c.KeyValue})
	case authBasic:
		missing = missingMembers(map[string]string{"Username": c.Username, "Password": c.Password})
	case authOAuth:
		missing = missingMembers(map[string]string{
			"ClientParameters.ClientID": c.ClientID, "ClientParameters.ClientSecret": c.ClientSecret,
			"AuthorizationEndpoint": c.Endpoint, "HttpMethod": c.Method,
		})
	}

	if len(missing) > 0 {
		return invalid("AuthParameters must set %s for the AuthorizationType %s.", strings.Join(missing, ", "), c.Type)
	}

	return nil
}

func missingMembers(members map[string]string) []string {
	var out []string
	for name, v := range members {
		if v == "" {
			out = append(out, name)
		}
	}

	slices.Sort(out)
	return out
}

// parameter is one of the three kinds of an API's HTTP parameter.
type parameter struct {
	key, value *string
	secret     *bool
}

// newParameters builds a connection's parameters from a request. A parameter without a value keeps the value
// the same key had in old, which is how an update leaves a secret in place, as Describe never returns it.
func newParameters(in *api.ConnectionHttpParameters, old *httpParameters) (httpParameters, error) {
	var headers, query, body []parameter
	for i := range in.HeaderParameters {
		p := &in.HeaderParameters[i]
		headers = append(headers, parameter{p.Key, p.Value, p.IsValueSecret})
	}

	for i := range in.QueryStringParameters {
		p := &in.QueryStringParameters[i]
		query = append(query, parameter{p.Key, p.Value, p.IsValueSecret})
	}

	for i := range in.BodyParameters {
		p := &in.BodyParameters[i]
		body = append(body, parameter{p.Key, p.Value, p.IsValueSecret})
	}

	var out httpParameters
	var err error
	if out.Headers, err = convertParameters("header", headers, old.Headers); err != nil {
		return out, err
	}

	if out.Query, err = convertParameters("query string", query, old.Query); err != nil {
		return out, err
	}

	out.Body, err = convertParameters("body", body, old.Body)
	return out, err
}

func convertParameters(kind string, in []parameter, old []httpParameter) ([]httpParameter, error) {
	var out []httpParameter
	for _, p := range in {
		key := common.Value(p.key)
		if err := checkText("A "+kind+" parameter key", key, maxAuthText); err != nil {
			return nil, err
		}

		if kind == "header" && !headerName.MatchString(key) {
			return nil, invalid("The header parameter key %s is not a valid HTTP header name.", key)
		}

		if slices.ContainsFunc(out, func(o httpParameter) bool { return o.Key == key }) {
			return nil, invalid("The %s parameter %s is given twice.", kind, key)
		}

		converted := httpParameter{Key: key, Secret: common.Value(p.secret)}
		if p.value != nil {
			converted.Value = *p.value
		} else if i := slices.IndexFunc(old, func(o httpParameter) bool { return o.Key == key }); i >= 0 {
			converted.Value = old[i].Value
		} else {
			return nil, invalid("The %s parameter %s needs a Value.", kind, key)
		}

		out = append(out, converted)
	}

	return out, nil
}

// describe renders parameters without the values of secret ones.
func (p *httpParameters) describe() *api.ConnectionHttpParameters {
	if len(p.Headers)+len(p.Query)+len(p.Body) == 0 {
		return nil
	}

	value := func(q httpParameter) *string {
		if q.Secret {
			return nil
		}

		return new(q.Value)
	}

	out := &api.ConnectionHttpParameters{}
	for _, q := range p.Headers {
		out.HeaderParameters = append(out.HeaderParameters, api.ConnectionHeaderParameter{
			Key: new(q.Key), Value: value(q), IsValueSecret: new(q.Secret),
		})
	}

	for _, q := range p.Query {
		out.QueryStringParameters = append(out.QueryStringParameters, api.ConnectionQueryStringParameter{
			Key: new(q.Key), Value: value(q), IsValueSecret: new(q.Secret),
		})
	}

	for _, q := range p.Body {
		out.BodyParameters = append(out.BodyParameters, api.ConnectionBodyParameter{
			Key: new(q.Key), Value: value(q), IsValueSecret: new(q.Secret),
		})
	}

	return out
}

// authParameters renders the authorization without its secrets.
func (c *connection) authParameters() *api.ConnectionAuthResponseParameters {
	out := &api.ConnectionAuthResponseParameters{InvocationHttpParameters: c.Parameters.describe()}
	switch c.Type {
	case authAPIKey:
		out.ApiKeyAuthParameters = &api.ConnectionApiKeyAuthResponseParameters{ApiKeyName: new(c.KeyName)}
	case authBasic:
		out.BasicAuthParameters = &api.ConnectionBasicAuthResponseParameters{Username: new(c.Username)}
	case authOAuth:
		out.OAuthParameters = &api.ConnectionOAuthResponseParameters{
			ClientParameters:      &api.ConnectionOAuthClientResponseParameters{ClientID: new(c.ClientID)},
			AuthorizationEndpoint: new(c.Endpoint),
			HttpMethod:            new(c.Method),
			OAuthHttpParameters:   c.OAuthParameters.describe(),
		}
	}

	return out
}

// list renders a connection as ListConnections does.
func (c *connection) list(scope request.Scope) api.Connection {
	return api.Connection{
		ConnectionArn: new(c.arn(scope)), Name: new(c.Name), ConnectionState: new(c.State),
		AuthorizationType: new(c.Type), CreationTime: seconds(c.Created), LastModifiedTime: seconds(c.Modified),
		LastAuthorizedTime: seconds(c.Authorized),
	}
}

func (s *Service) CreateConnection(
	ctx context.Context,
	in *api.CreateConnectionRequest,
) (*api.CreateConnectionResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.Name)
	if !resourceName.MatchString(name) {
		return nil, invalid("Connection name must be 1 to 64 characters from [.-_A-Za-z0-9].")
	}

	if len(common.Value(in.Description)) > 512 {
		return nil, invalid("Description must be at most 512 characters.")
	}

	if in.AuthParameters == nil {
		return nil, invalid("AuthParameters is required.")
	}

	now := s.now().UnixMilli()
	c := &connection{
		Name: name, ID: newID(), Description: common.Value(in.Description), State: connectionAuthorized,
		Created: now, Modified: now, Authorized: now,
	}
	auth := createAuth(in.AuthParameters)
	if applyErr := c.apply(common.Value(in.AuthorizationType), &auth); applyErr != nil {
		return nil, applyErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, exists := tx.Get(connectionPrefix(m.Scope) + name); exists {
			return awserror.New("ResourceAlreadyExistsException", "Connection "+name+" already exists.")
		}

		return storage.Write(tx, connectionPrefix(m.Scope)+name, c)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateConnectionResponse{
		ConnectionArn: new(c.arn(m.Scope)), ConnectionState: new(c.State), CreationTime: seconds(c.Created),
		LastModifiedTime: seconds(c.Modified),
	}, nil
}

func (s *Service) DescribeConnection(
	ctx context.Context,
	in *api.DescribeConnectionRequest,
) (*api.DescribeConnectionResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var c *connection
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		c, loadErr = loadConnection(tx, m.Scope, common.Value(in.Name))
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	out := &api.DescribeConnectionResponse{
		ConnectionArn:     new(c.arn(m.Scope)),
		Name:              new(c.Name),
		ConnectionState:   new(c.State),
		AuthorizationType: new(c.Type),
		SecretArn:         new(c.secretARN(m.Scope)),
		AuthParameters:    c.authParameters(),
		CreationTime: seconds(
			c.Created,
		),
		LastModifiedTime:   seconds(c.Modified),
		LastAuthorizedTime: seconds(c.Authorized),
	}
	if c.Description != "" {
		out.Description = new(c.Description)
	}

	return out, nil
}

// ListConnections lists the caller's connections in name order, by name prefix or state.
func (s *Service) ListConnections(
	ctx context.Context,
	in *api.ListConnectionsRequest,
) (*api.ListConnectionsResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	after, size, err := page(in.NextToken, in.Limit)
	if err != nil {
		return nil, err
	}

	out := &api.ListConnectionsResponse{Connections: []api.Connection{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(connectionPrefix(m.Scope) + common.Value(in.NamePrefix)) {
			c, _, loadErr := storage.Read[connection](tx, k)
			if loadErr != nil {
				return loadErr
			}

			switch {
			case c.Name <= after:
			case in.ConnectionState != nil && *in.ConnectionState != c.State:
			case len(out.Connections) == size:
				out.NextToken = nextToken(*out.Connections[size-1].Name)
				return nil
			default:
				out.Connections = append(out.Connections, c.list(m.Scope))
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// UpdateConnection changes a connection. Members the request leaves out keep their value, and a request with
// AuthParameters authorizes the connection again.
func (s *Service) UpdateConnection(
	ctx context.Context,
	in *api.UpdateConnectionRequest,
) (*api.UpdateConnectionResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if len(common.Value(in.Description)) > 512 {
		return nil, invalid("Description must be at most 512 characters.")
	}

	var c *connection
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var loadErr error
		if c, loadErr = loadConnection(tx, m.Scope, common.Value(in.Name)); loadErr != nil {
			return loadErr
		}

		now := s.now().UnixMilli()
		c.Modified = now
		if in.Description != nil {
			c.Description = *in.Description
		}

		if in.AuthParameters != nil || in.AuthorizationType != nil {
			auth := authInput{}
			if in.AuthParameters != nil {
				auth = updateAuth(in.AuthParameters)
			}

			if applyErr := c.apply(common.Value(common.OrElse(in.AuthorizationType, &c.Type)), &auth); applyErr != nil {
				return applyErr
			}

			c.State, c.Authorized = connectionAuthorized, now
		}

		return storage.Write(tx, connectionPrefix(m.Scope)+c.Name, c)
	})
	if err != nil {
		return nil, err
	}

	s.tokens.drop(c.arn(m.Scope))
	return &api.UpdateConnectionResponse{
		ConnectionArn: new(c.arn(m.Scope)), ConnectionState: new(c.State), CreationTime: seconds(c.Created),
		LastModifiedTime: seconds(c.Modified), LastAuthorizedTime: seconds(c.Authorized),
	}, nil
}

// DeleteConnection deletes a connection. API destinations that use it fail their deliveries.
func (s *Service) DeleteConnection(
	ctx context.Context,
	in *api.DeleteConnectionRequest,
) (*api.DeleteConnectionResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var c *connection
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var loadErr error
		if c, loadErr = loadConnection(tx, m.Scope, common.Value(in.Name)); loadErr != nil {
			return loadErr
		}

		tx.Delete(connectionPrefix(m.Scope) + c.Name)
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.tokens.drop(c.arn(m.Scope))
	return &api.DeleteConnectionResponse{
		ConnectionArn: new(c.arn(m.Scope)), ConnectionState: new(c.State), CreationTime: seconds(c.Created),
		LastModifiedTime: seconds(c.Modified), LastAuthorizedTime: seconds(c.Authorized),
	}, nil
}

// DeauthorizeConnection removes the connection's credentials, as AWS does, so UpdateConnection must give them
// again before an API destination can use the connection.
func (s *Service) DeauthorizeConnection(
	ctx context.Context,
	in *api.DeauthorizeConnectionRequest,
) (*api.DeauthorizeConnectionResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var c *connection
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var loadErr error
		if c, loadErr = loadConnection(tx, m.Scope, common.Value(in.Name)); loadErr != nil {
			return loadErr
		}

		c.State, c.Modified = connectionDeauthorized, s.now().UnixMilli()
		c.KeyValue, c.Password, c.ClientSecret = "", "", ""
		c.Parameters, c.OAuthParameters = c.Parameters.withoutSecrets(), c.OAuthParameters.withoutSecrets()
		return storage.Write(tx, connectionPrefix(m.Scope)+c.Name, c)
	})
	if err != nil {
		return nil, err
	}

	s.tokens.drop(c.arn(m.Scope))
	return &api.DeauthorizeConnectionResponse{
		ConnectionArn: new(c.arn(m.Scope)), ConnectionState: new(c.State), CreationTime: seconds(c.Created),
		LastModifiedTime: seconds(c.Modified), LastAuthorizedTime: seconds(c.Authorized),
	}, nil
}

// withoutSecrets keeps the parameters that are not secret.
func (p httpParameters) withoutSecrets() httpParameters {
	keep := func(in []httpParameter) []httpParameter {
		return slices.DeleteFunc(slices.Clone(in), func(q httpParameter) bool { return q.Secret })
	}

	return httpParameters{Headers: keep(p.Headers), Query: keep(p.Query), Body: keep(p.Body)}
}
