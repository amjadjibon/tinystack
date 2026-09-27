package cognitoidp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	challengeNewPassword = "NEW_PASSWORD_REQUIRED"
	paramUsername        = "USERNAME"
	paramSecretHash      = "SECRET_HASH"
	attributePrefix      = "userAttributes."
)

// authSession is a pending challenge's record, under session/<pool ID>/<ID>.
type authSession struct {
	Expires   time.Time
	Username  string
	Client    string
	Challenge string
}

func sessionKey(scope request.Scope, pool, id string) string {
	return prefix(scope, "session") + pool + "/" + id
}

// outcome is what every sign-in operation returns, with their shared wire
// names: tokens, or a challenge to answer.
type outcome struct {
	ChallengeName        *string                       `json:",omitempty"`
	Session              *string                       `json:",omitempty"`
	ChallengeParameters  map[string]string             `json:",omitempty"`
	AuthenticationResult *api.AuthenticationResultType `json:",omitempty"`
}

// byClient runs fn in a transaction with the pool and app client clientID
// names, for the operations users call unsigned, which name no account.
func (s *Service) byClient(
	ctx context.Context,
	clientID *string,
	fn func(tx storage.WriteTx, scope request.Scope, p *pool, c *client) error,
) error {
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		index, ok, err := read[clientIndex](tx, clientIndexKey(value(clientID)))
		if err != nil {
			return err
		}

		if !ok {
			return notFound("User pool client %s does not exist.", value(clientID))
		}

		scope := request.Scope{AccountID: index.Account, Region: index.Region}
		p, err := loadPool(tx, scope, &index.Pool)
		if err != nil {
			return err
		}

		c, err := loadClient(tx, scope, p, clientID)
		if err != nil {
			return err
		}

		return fn(tx, scope, p, c)
	})
}

// byAdminClient is byClient for the admin operations, which name the pool and
// are signed by its account.
func (s *Service) byAdminClient(
	ctx context.Context,
	poolID, clientID *string,
	fn func(tx storage.WriteTx, scope request.Scope, p *pool, c *client) error,
) error {
	return s.update(ctx, poolID, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		c, err := loadClient(tx, scope, p, clientID)
		if err != nil {
			return err
		}

		return fn(tx, scope, p, c)
	})
}

// secretHash is the SECRET_HASH a client with a secret sends for username.
func secretHash(c *client, username string) string {
	mac := hmac.New(sha256.New, []byte(c.Secret))
	mac.Write([]byte(username + c.ID))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// checkSecretHash requires a client with a secret to prove it for one of
// usernames.
func checkSecretHash(c *client, hash *string, usernames ...string) error {
	if c.Secret == "" {
		return nil
	}

	if value(hash) == "" {
		return notAuthorized("Client " + c.ID + " is configured with secret but SECRET_HASH was not received")
	}

	for _, name := range usernames {
		if hmac.Equal([]byte(secretHash(c, name)), []byte(*hash)) {
			return nil
		}
	}

	return notAuthorized("Unable to verify secret hash for client " + c.ID)
}

func requireFlow(c *client, flow, name string) error {
	if !slices.Contains(c.Settings.flows(), flow) {
		return invalidParameter("%s flow not enabled for this client", name)
	}

	return nil
}

// authenticate runs an auth flow for c.
func (s *Service) authenticate(
	ctx context.Context, tx storage.WriteTx, scope request.Scope, p *pool, c *client, flow string,
	params map[string]string, admin bool,
) (*outcome, error) {
	switch {
	case flow == "REFRESH_TOKEN_AUTH" || flow == "REFRESH_TOKEN":
		if err := requireFlow(c, flowRefresh, flow); err != nil {
			return nil, err
		}

		return s.refresh(ctx, tx, scope, p, c, params)
	case !admin && flow == legacyUserPassword:
		if err := requireFlow(c, flowUserPassword, flow); err != nil {
			return nil, err
		}
	case admin && (flow == "ADMIN_USER_PASSWORD_AUTH" || flow == legacyAdminPassword):
		if err := requireFlow(c, flowAdminPassword, flow); err != nil {
			return nil, err
		}
	case flow == "USER_SRP_AUTH" || flow == "CUSTOM_AUTH" || flow == "USER_AUTH":
		return nil, invalidParameter("TinyStack does not support the %s flow; use a password flow.", flow)
	default:
		return nil, invalidParameter("Invalid AuthFlow %s.", flow)
	}

	return s.passwordAuth(ctx, tx, scope, p, c, params)
}

// findSigningIn returns the user signing in as name, hiding whether a user
// exists when the client asks to.
func findSigningIn(tx storage.ReadTx, scope request.Scope, p *pool, c *client, name string) (*user, error) {
	u, err := findUser(tx, scope, p, &name)
	if awserror.From(err).Code == "UserNotFoundException" && value(c.Settings.PreventUserExistenceErrors) == "ENABLED" {
		return nil, notAuthorized("Incorrect username or password.")
	}

	return u, err
}

func (s *Service) passwordAuth(
	ctx context.Context, tx storage.WriteTx, scope request.Scope, p *pool, c *client, params map[string]string,
) (*outcome, error) {
	name, password := params[paramUsername], params["PASSWORD"]
	if name == "" || password == "" {
		return nil, invalidParameter("Missing required parameter USERNAME or PASSWORD")
	}

	u, err := findSigningIn(tx, scope, p, c, name)
	if err != nil {
		return nil, err
	}

	hash := params[paramSecretHash]
	if hashErr := checkSecretHash(c, &hash, name, u.Username); hashErr != nil {
		return nil, hashErr
	}

	now := s.now().UTC()
	switch {
	case u.Disabled:
		return nil, notAuthorized("User is disabled.")
	case !checkPassword(u.Password, password):
		return nil, notAuthorized("Incorrect username or password.")
	case u.Status == statusUnconfirmed:
		return nil, awserror.New("UserNotConfirmedException", "User is not confirmed.")
	case u.Status == statusReset:
		return nil, awserror.New("PasswordResetRequiredException", "Password reset required for the user")
	case u.Status == statusForceChange && !u.PasswordExpires.IsZero() && !now.Before(u.PasswordExpires):
		return nil, notAuthorized("Temporary password has expired and must be reset by an administrator.")
	case u.Status == statusForceChange:
		return challengeNew(tx, scope, p, c, u, now)
	}

	result, err := s.signIn(ctx, tx, scope, p, c, u)
	if err != nil {
		return nil, err
	}

	return &outcome{AuthenticationResult: result, ChallengeParameters: map[string]string{}}, nil
}

// challengeNew asks a user signing in with a temporary password for a new one.
func challengeNew(
	tx storage.WriteTx,
	scope request.Scope,
	p *pool,
	c *client,
	u *user,
	now time.Time,
) (*outcome, error) {
	id := randomString(alphaNum, 512)
	sess := authSession{
		Username: u.Username, Client: c.ID, Challenge: challengeNewPassword,
		Expires: now.Add(time.Duration(value(c.Settings.AuthSessionValidity)) * time.Minute),
	}
	if err := write(tx, sessionKey(scope, p.ID, id), sess); err != nil {
		return nil, err
	}

	current := map[string]string{}
	for k, v := range u.Attributes {
		if k != attrSub {
			current[k] = v
		}
	}

	required := []string{}
	for i := range p.Schema {
		a := &p.Schema[i]
		if value(a.Required) && *a.Name != attrSub && u.Attributes[*a.Name] == "" {
			required = append(required, attributePrefix+*a.Name)
		}
	}

	attrs, err := json.Marshal(current)
	if err != nil {
		return nil, err
	}

	missing, err := json.Marshal(required)
	if err != nil {
		return nil, err
	}

	return &outcome{
		ChallengeName: new(challengeNewPassword), Session: &id,
		ChallengeParameters: map[string]string{
			"USER_ID_FOR_SRP": u.Username, "userAttributes": string(attrs), "requiredAttributes": string(missing),
		},
	}, nil
}

// refresh issues new access and ID tokens for a refresh token's session.
func (s *Service) refresh(
	ctx context.Context, tx storage.WriteTx, scope request.Scope, p *pool, c *client, params map[string]string,
) (*outcome, error) {
	token := params["REFRESH_TOKEN"]
	r, ok, err := read[refresh](tx, refreshKey(scope, p.ID, token))
	switch {
	case err != nil:
		return nil, err
	case token == "" || !ok || r.Client != c.ID:
		return nil, notAuthorized("Invalid Refresh Token")
	case !s.now().Before(r.Expires):
		return nil, notAuthorized("Refresh Token has expired")
	}

	u, ok, err := read[user](tx, userKey(scope, p, r.Username))
	switch {
	case err != nil:
		return nil, err
	case !ok:
		return nil, userNotFound()
	case u.Disabled:
		return nil, notAuthorized("User is disabled.")
	case !slices.Contains(u.Sessions, r.Origin):
		return nil, notAuthorized("Refresh Token has been revoked")
	}

	hash := params[paramSecretHash]
	if hashErr := checkSecretHash(c, &hash, u.Username, u.Attributes[attrSub], params[paramUsername]); hashErr != nil {
		return nil, hashErr
	}

	key, _, err := signingKey(p)
	if err != nil {
		return nil, err
	}

	result, err := tokens(ctx, key, p, c, u, session{auth: r.AuthTime, origin: r.Origin}, s.now().UTC())
	if err != nil {
		return nil, err
	}

	return &outcome{AuthenticationResult: result, ChallengeParameters: map[string]string{}}, nil
}

// answer completes a NEW_PASSWORD_REQUIRED challenge.
func (s *Service) answer(
	ctx context.Context, tx storage.WriteTx, scope request.Scope, p *pool, c *client, name, id *string,
	responses map[string]string,
) (*outcome, error) {
	if value(name) != challengeNewPassword {
		return nil, invalidParameter("TinyStack answers only the %s challenge.", challengeNewPassword)
	}

	expired := notAuthorized("Invalid session for the user, session is expired.")
	sess, ok, err := read[authSession](tx, sessionKey(scope, p.ID, value(id)))
	now := s.now().UTC()
	switch {
	case err != nil:
		return nil, err
	case value(id) == "" || !ok || sess.Client != c.ID || sess.Challenge != *name || !now.Before(sess.Expires):
		return nil, expired
	}

	u, err := findUser(tx, scope, p, new(responses[paramUsername]))
	if err != nil {
		return nil, err
	}

	if u.Username != sess.Username {
		return nil, expired
	}

	hash := responses[paramSecretHash]
	if hashErr := checkSecretHash(c, &hash, responses[paramUsername], u.Username); hashErr != nil {
		return nil, hashErr
	}

	password := new(responses["NEW_PASSWORD"])
	if policyErr := checkPolicy(p, password); policyErr != nil {
		return nil, policyErr
	}

	var attrs []api.AttributeType
	for k, v := range responses {
		if attr, isAttr := strings.CutPrefix(k, attributePrefix); isAttr {
			attrs = append(attrs, api.AttributeType{Name: new(attr), Value: new(v)})
		}
	}

	before := *u
	before.Attributes = cloneMap(u.Attributes)
	if _, writeErr := clientWrite(p, c, u, attrs); writeErr != nil {
		return nil, writeErr
	}

	if reqErr := checkRequired(p, u); reqErr != nil {
		return nil, reqErr
	}

	if setErr := setPassword(p, u, *password, true, now); setErr != nil {
		return nil, setErr
	}

	tx.Delete(sessionKey(scope, p.ID, *id))
	if saveErr := saveUser(tx, scope, p, &before, u, false); saveErr != nil {
		return nil, saveErr
	}

	result, err := s.signIn(ctx, tx, scope, p, c, u)
	if err != nil {
		return nil, err
	}

	return &outcome{AuthenticationResult: result, ChallengeParameters: map[string]string{}}, nil
}

// respondWith converts an outcome to a sign-in operation's response.
func respondWith[T any](o *outcome, err error) (*T, error) {
	if err != nil {
		return nil, err
	}

	out, err := convert[T](o)
	if err != nil {
		return nil, err
	}

	return &out, nil
}

func (s *Service) InitiateAuth(ctx context.Context, in *api.InitiateAuthRequest) (*api.InitiateAuthResponse, error) {
	var out *outcome
	err := s.byClient(ctx, in.ClientId, func(tx storage.WriteTx, scope request.Scope, p *pool, c *client) error {
		var authErr error
		out, authErr = s.authenticate(ctx, tx, scope, p, c, value(in.AuthFlow), in.AuthParameters, false)
		return authErr
	})

	return respondWith[api.InitiateAuthResponse](out, err)
}

func (s *Service) AdminInitiateAuth(
	ctx context.Context,
	in *api.AdminInitiateAuthRequest,
) (*api.AdminInitiateAuthResponse, error) {
	var out *outcome
	err := s.byAdminClient(ctx, in.UserPoolId, in.ClientId,
		func(tx storage.WriteTx, scope request.Scope, p *pool, c *client) error {
			var authErr error
			out, authErr = s.authenticate(ctx, tx, scope, p, c, value(in.AuthFlow), in.AuthParameters, true)
			return authErr
		})

	return respondWith[api.AdminInitiateAuthResponse](out, err)
}

func (s *Service) RespondToAuthChallenge(
	ctx context.Context,
	in *api.RespondToAuthChallengeRequest,
) (*api.RespondToAuthChallengeResponse, error) {
	var out *outcome
	err := s.byClient(ctx, in.ClientId, func(tx storage.WriteTx, scope request.Scope, p *pool, c *client) error {
		var answerErr error
		out, answerErr = s.answer(ctx, tx, scope, p, c, in.ChallengeName, in.Session, in.ChallengeResponses)
		return answerErr
	})

	return respondWith[api.RespondToAuthChallengeResponse](out, err)
}

func (s *Service) AdminRespondToAuthChallenge(
	ctx context.Context,
	in *api.AdminRespondToAuthChallengeRequest,
) (*api.AdminRespondToAuthChallengeResponse, error) {
	var out *outcome
	err := s.byAdminClient(ctx, in.UserPoolId, in.ClientId,
		func(tx storage.WriteTx, scope request.Scope, p *pool, c *client) error {
			var answerErr error
			out, answerErr = s.answer(ctx, tx, scope, p, c, in.ChallengeName, in.Session, in.ChallengeResponses)
			return answerErr
		})

	return respondWith[api.AdminRespondToAuthChallengeResponse](out, err)
}
