package cognitoidp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const hostedCookieTTL = time.Hour

// HostedResult is a redirect or an escaped server-rendered form, rendered by the gateway.
type HostedResult struct {
	Required     []string
	Redirect     string
	Transaction  string
	CSRF         string
	Browser      string
	Session      string
	Challenge    string
	Username     string
	Message      string
	TOTPSecret   string
	Providers    []string
	ClearSession bool
}

type authorization struct {
	Client    string
	Redirect  string
	Response  string
	State     string
	Nonce     string
	Challenge string
	Method    string
	Provider  string
	Prompt    string
	Hint      string
	Scopes    []string
}

type hostedTransaction struct {
	Authorization authorization
	Expires       time.Time
	CSRF          string
	Browser       string
	Challenge     string
	AuthSession   string
	Username      string
	TOTPSecret    string
	Required      []string
}

type browserSession struct {
	Expires  time.Time
	AuthTime time.Time
	Username string
	Sub      string
	Origin   string
	Provider string
}

type authorizationCode struct {
	Expires       time.Time
	AuthTime      time.Time
	Username      string
	Sub           string
	Origin        string
	Refresh       string
	Authorization authorization
}

func opaqueHash(value string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(value))) }

func hostedKey(scope request.Scope, kind, pool, id string) string {
	return scope.Key(service, kind, pool+"/"+opaqueHash(id))
}

var pkceValue = regexp.MustCompile(`^[A-Za-z0-9._~\-]{43,128}$`)

func parseAuthorization(c *client, values url.Values) (parsed *authorization, failure string) {
	redirect := values.Get("redirect_uri")
	if redirect == "" && len(c.Settings.SupportedIdentityProviders) == 1 {
		redirect = common.Value(c.Settings.DefaultRedirectURI)
	}

	redirectURL, err := url.Parse(redirect)
	if err != nil || !redirectURL.IsAbs() || redirectURL.Fragment != "" ||
		!slices.Contains(c.Settings.CallbackURLs, redirect) {
		return nil, invalidRedirectURI
	}

	a := &authorization{
		Client:   c.ID,
		Redirect: redirect,
		Response: values.Get("response_type"),
		State:    values.Get("state"),
		Nonce: values.Get(
			"nonce",
		),
		Challenge: values.Get("code_challenge"),
		Method:    values.Get("code_challenge_method"),
		Provider:  values.Get("identity_provider"),
		Prompt:    values.Get("prompt"),
		Hint:      values.Get("login_hint"),
		Scopes:    strings.Fields(values.Get(claimScope)),
	}

	if a.Response != flowCode && a.Response != responseToken {
		return a, "unsupported_response_type"
	}

	flow := a.Response
	if flow == responseToken {
		flow = flowImplicit
	}

	if !common.Value(c.Settings.AllowedOAuthFlowsUserPoolClient) ||
		!slices.Contains(c.Settings.AllowedOAuthFlows, flow) {
		return a, unauthorizedClient
	}

	if len(a.Scopes) == 0 {
		a.Scopes = slices.Clone(c.Settings.AllowedOAuthScopes)
	}

	for _, sc := range a.Scopes {
		if !slices.Contains(c.Settings.AllowedOAuthScopes, sc) {
			return a, invalidScope
		}
	}

	if !slices.Contains(a.Scopes, "openid") && (slices.Contains(a.Scopes, attrEmail) ||
		slices.Contains(a.Scopes, "phone") || slices.Contains(a.Scopes, scopeProfile)) {
		return a, invalidScope
	}

	if a.Challenge != "" && a.Method == "" {
		a.Method = pkcePlain
	}

	if (a.Challenge == "" && a.Method != "") || (a.Challenge != "" &&
		(!pkceValue.MatchString(a.Challenge) || (a.Method != pkceS256 && a.Method != pkcePlain) || a.Response != flowCode)) {
		return a, invalidRequest
	}

	for _, prompt := range strings.Fields(a.Prompt) {
		if !slices.Contains([]string{"login", promptNone, "consent", "select_account"}, prompt) {
			return a, invalidRequest
		}
	}

	if slices.Contains(strings.Fields(a.Prompt), promptNone) && a.Prompt != promptNone {
		return a, invalidRequest
	}

	allowed := []string{
		claimClientID, "redirect_uri", "response_type", claimScope, "state", "nonce", "code_challenge",
		"code_challenge_method", "identity_provider", "idp_identifier", "prompt", "login_hint",
	}

	for k, v := range values {
		if !slices.Contains(allowed, k) || len(v) != 1 {
			return a, invalidRequest
		}
	}

	return a, ""
}

func authorizationRedirect(a *authorization, values url.Values) string {
	if a.State != "" {
		values.Set("state", a.State)
	}

	u, _ := url.Parse(a.Redirect)
	if a.Response == responseToken {
		u.Fragment = values.Encode()
	} else {
		q := u.Query()
		for k, v := range values {
			q[k] = v
		}

		u.RawQuery = q.Encode()
	}

	return u.String()
}

func authorizationFailure(a *authorization, code, message string) *HostedResult {
	return &HostedResult{
		Redirect: authorizationRedirect(a, url.Values{"error": {code}, "error_description": {message}}),
	}
}

func providerAllowed(c *client, name string) bool {
	if len(c.Settings.SupportedIdentityProviders) == 0 {
		return name == providerCognito
	}

	return slices.Contains(c.Settings.SupportedIdentityProviders, name)
}

// Authorize validates the client and callback before any redirect, and binds a fresh form transaction to its browser.
func (s *Service) Authorize(
	ctx context.Context, poolID string, values url.Values, browser, cookie string,
) (*HostedResult, error) {
	var result *HostedResult
	var idp *api.IdentityProviderType
	var pending *hostedTransaction
	var transactionID string
	var g *grant
	err := s.byClient(
		ctx,
		new(values.Get(claimClientID)),
		func(tx storage.WriteTx, scope request.Scope, p *pool, c *client) error {
			if p.ID != poolID {
				return invalidParameter("Invalid client for this pool.")
			}

			a, failure := parseAuthorization(c, values)
			if a == nil {
				return invalidParameter("Invalid redirect_uri.")
			}

			if failure != "" {
				result = authorizationFailure(a, failure, "Authorization request rejected.")
				return nil
			}

			if identifier := values.Get("idp_identifier"); identifier != "" {
				if a.Provider != "" {
					result = authorizationFailure(a, invalidRequest, "Choose one identity provider.")
					return nil
				}

				records, err := storage.Records[api.IdentityProviderType](
					tx,
					scope.Prefix(service, "provider")+p.ID+"/",
				)
				if err != nil {
					return err
				}

				for _, record := range records {
					if slices.Contains(record.IdpIdentifiers, identifier) {
						a.Provider = common.Value(record.ProviderName)
					}
				}

				if a.Provider == "" {
					result = authorizationFailure(a, invalidRequest, "Unknown identity provider.")
					return nil
				}
			}

			if a.Provider != "" && !providerAllowed(c, a.Provider) {
				result = authorizationFailure(a, invalidRequest, "Identity provider is not enabled for this client.")
				return nil
			}

			if !slices.Contains(strings.Fields(a.Prompt), "login") && cookie != "" {
				sess, ok, err := storage.Read[browserSession](tx, hostedKey(scope, "browsersession", p.ID, cookie))
				if err != nil {
					return err
				}

				if ok && s.now().Before(sess.Expires) && providerAllowed(c, sess.Provider) &&
					(a.Provider == "" || a.Provider == sess.Provider) {
					u, ok, err := storage.Read[user](tx, userKey(scope, p, sess.Username))
					if err != nil {
						return err
					}

					if ok && !u.Disabled && u.Attributes[attrSub] == sess.Sub &&
						slices.Contains(u.Sessions, sess.Origin) {
						g, err = s.signIn(ctx, tx, scope, p, c, u, sourceAuth)
						if err != nil {
							return err
						}

						g.access[claimAuthTime], g.id[claimAuthTime] = sess.AuthTime.Unix(), sess.AuthTime.Unix()
						g.postAuth = nil
						result, err = s.completeAuthorization(tx, scope, p, a, g)
						return err
					}
				}
			}

			if a.Prompt == promptNone {
				result = authorizationFailure(a, "login_required", "Interactive sign-in required.")
				return nil
			}

			transactionID = common.RandomID(64, alphaNum)
			if browser == "" {
				browser = common.RandomID(64, alphaNum)
			}

			pending = &hostedTransaction{
				Authorization: *a, Browser: opaqueHash(browser), CSRF: common.RandomID(64, alphaNum),
				Expires: s.now().UTC().Add(time.Duration(common.Value(c.Settings.AuthSessionValidity)) * time.Minute),
			}

			if a.Provider != "" && a.Provider != providerCognito {
				var err error
				idp, err = loadProvider(tx, scope, p, new(a.Provider))
				if err != nil {
					result = authorizationFailure(a, invalidRequest, "Unknown identity provider.")
					return nil //nolint:nilerr // OAuth redirect.
				}

				if common.Value(idp.ProviderType) != providerOIDC {
					result = authorizationFailure(
						a,
						invalidRequest,
						"Sign-in supports OIDC providers only; this provider is records only.",
					)
					return nil
				}
			}

			result = hostedForm(transactionID, pending, c)
			result.Browser = browser
			return storage.Write(tx, hostedKey(scope, "hosted", p.ID, transactionID), pending)
		},
	)
	if err != nil {
		return nil, err
	}

	if g != nil {
		return s.finishAuthorization(ctx, result, g)
	}

	if idp != nil && result.Redirect == "" {
		return s.beginFederation(ctx, poolID, transactionID, pending, idp, result)
	}

	return result, nil
}

func hostedForm(id string, t *hostedTransaction, c *client) *HostedResult {
	providers := []string{}
	for _, name := range c.Settings.SupportedIdentityProviders {
		if name != providerCognito {
			providers = append(providers, name)
		}
	}

	username := t.Username
	if username == "" {
		username = t.Authorization.Hint
	}

	return &HostedResult{
		Transaction: id, CSRF: t.CSRF, Challenge: t.Challenge, Username: username,
		Required: t.Required, Providers: providers, TOTPSecret: t.TOTPSecret,
	}
}

// HostedLogin consumes each valid CSRF form once; native password/MFA/challenge handling is shared with the API.
func (s *Service) HostedLogin(
	ctx context.Context, poolID string, form url.Values, browser, oldSession string,
) (*HostedResult, error) {
	var result *HostedResult
	var out *outcome
	var idp *api.IdentityProviderType
	var pending *hostedTransaction
	id := form.Get("transaction")
	var g *grant
	err := s.runAuth(ctx, func(ctx context.Context) error {
		return s.store.Update(ctx, func(tx storage.WriteTx) error {
			scope, p, err := hostedPool(tx, poolID)
			if err != nil {
				return err
			}

			t, ok, err := storage.Read[hostedTransaction](tx, hostedKey(scope, "hosted", p.ID, id))
			if err != nil {
				return err
			}

			if !ok || !s.now().Before(t.Expires) || browser == "" ||
				!hmac.Equal([]byte(t.Browser), []byte(opaqueHash(browser))) ||
				!hmac.Equal([]byte(t.CSRF), []byte(form.Get("csrf"))) {
				return invalidParameter("Invalid or expired sign-in form.")
			}

			c, err := loadClient(tx, scope, p, new(t.Authorization.Client))
			if err != nil {
				return err
			}

			t.CSRF = common.RandomID(64, alphaNum)
			// Recheck callback, flow and scopes against live settings before any authentication or redirect.
			if failure := liveAuthorization(c, &t.Authorization); failure != "" {
				tx.Delete(hostedKey(scope, "hosted", p.ID, id))
				if failure == invalidRedirectURI {
					return invalidParameter("The callback is no longer allowed.")
				}

				result = authorizationFailure(&t.Authorization, failure, "Client settings changed.")
				return nil
			}

			if provider := form.Get("identity_provider"); provider != "" {
				if t.Challenge != "" || !providerAllowed(c, provider) {
					return invalidParameter("Identity provider is not allowed.")
				}

				idp, err = loadProvider(tx, scope, p, &provider)
				if err != nil {
					return err
				}

				if common.Value(idp.ProviderType) != providerOIDC {
					return invalidParameter("Sign-in supports OIDC providers only.")
				}

				t.Authorization.Provider = provider
				pending = t
				result = hostedForm(id, t, c)
				return storage.Write(tx, hostedKey(scope, "hosted", p.ID, id), t)
			}

			if !providerAllowed(c, providerCognito) {
				return invalidParameter("Local sign-in is not enabled for this client.")
			}

			if t.Challenge == "" {
				t.Username = form.Get("username")
				out, err = s.passwordAuth(ctx, tx, scope, p, c, map[string]string{
					paramUsername: t.Username,
					"PASSWORD":    form.Get("password"), paramSecretHash: secretHash(c, t.Username),
				})
			} else {
				if t.Challenge == challengeSetup {
					sess, ok, sessionErr := storage.Read[authSession](tx, sessionKey(scope, p.ID, t.AuthSession))
					if sessionErr != nil {
						return sessionErr
					}

					if !ok || !s.now().Before(sess.Expires) {
						return expiredSession()
					}

					u, userErr := findUser(tx, scope, p, &t.Username)
					if userErr != nil {
						return userErr
					}

					if u.Disabled || u.Attributes[attrSub] != sess.Sub || sess.Client != c.ID {
						return expiredSession()
					}

					if t.TOTPSecret != "" && sess.Setup == setupVerify {
						verified := &api.VerifySoftwareTokenResponse{}
						verifyErr := s.verifySoftwareToken(
							tx,
							&accessToken{scope: scope, pool: p, client: c, user: u},
							sess,
							&api.VerifySoftwareTokenRequest{UserCode: new(form.Get(flowCode))},
							verified,
						)
						if verifyErr != nil {
							if !validTOTP(sess.TOTPSecret, form.Get(flowCode), s.now()) {
								out, err = wrongCode(tx, scope, p, t.AuthSession, sess)
								if err != nil {
									return err
								}
							}

							result = hostedForm(id, t, c)
							result.Message = awserror.From(verifyErr).Message
							return storage.Write(tx, hostedKey(scope, "hosted", p.ID, id), t)
						}

						deleteAuthSession(tx, scope, p, t.AuthSession)
						t.AuthSession, t.TOTPSecret = common.Value(verified.Session), ""
						if writeErr := storage.Write(tx, userKey(scope, p, u.Username), u); writeErr != nil {
							return writeErr
						}
					}
				}

				responses := map[string]string{
					paramUsername: t.Username, paramSecretHash: secretHash(c, t.Username),
					"SMS_MFA_CODE": form.Get(flowCode), "SOFTWARE_TOKEN_MFA_CODE": form.Get(flowCode),
					"NEW_PASSWORD": form.Get("password"), "ANSWER": form.Get(flowCode),
				}

				for _, name := range t.Required {
					responses[name] = form.Get(name)
				}

				out, err = s.answer(ctx, tx, scope, p, c, new(t.Challenge), new(t.AuthSession), responses)
			}

			if err != nil {
				if errors.Is(err, errTrigger) {
					return err
				}

				result = hostedForm(id, t, c)
				result.Message = awserror.From(err).Message
				return storage.Write(tx, hostedKey(scope, "hosted", p.ID, id), t)
			}

			if out.failure != nil {
				result = hostedForm(id, t, c)
				result.Message = awserror.From(out.failure).Message
				return storage.Write(tx, hostedKey(scope, "hosted", p.ID, id), t)
			}

			if out.grant == nil {
				t.Challenge, t.AuthSession = common.Value(out.ChallengeName), common.Value(out.Session)
				if username := out.ChallengeParameters["USER_ID_FOR_SRP"]; username != "" {
					t.Username = username
				}

				if required := out.ChallengeParameters["requiredAttributes"]; required != "" {
					if decodeErr := json.Unmarshal([]byte(required), &t.Required); decodeErr != nil {
						return decodeErr
					}
				}

				if t.Challenge == challengeSetup && p.SoftwareTokenMFA {
					sess, ok, sessionErr := storage.Read[authSession](tx, sessionKey(scope, p.ID, t.AuthSession))
					if sessionErr != nil {
						return sessionErr
					}

					if !ok {
						return expiredSession()
					}

					u, userErr := findUser(tx, scope, p, new(sess.Username))
					if userErr != nil {
						return userErr
					}

					associated := &api.AssociateSoftwareTokenResponse{}
					if associateErr := s.associateSoftwareToken(
						tx,
						&accessToken{scope: scope, pool: p, client: c, user: u},
						sess,
						associated,
					); associateErr != nil {
						return associateErr
					}

					deleteAuthSession(tx, scope, p, t.AuthSession)
					t.AuthSession, t.TOTPSecret = common.Value(associated.Session), common.Value(associated.SecretCode)
					t.Username = sess.Username
				}

				result = hostedForm(id, t, c)
				return storage.Write(tx, hostedKey(scope, "hosted", p.ID, id), t)
			}

			g = out.grant
			result, err = s.completeAuthorization(tx, scope, p, &t.Authorization, g)
			if err != nil {
				return err
			}

			cookie := common.RandomID(64, alphaNum)
			sess := browserSession{
				Expires:  s.now().UTC().Add(hostedCookieTTL),
				AuthTime: s.now().UTC(),
				Username: g.access[claimUsername].(string),
				Sub:      g.access[attrSub].(string),
				Origin:   g.access["origin_jti"].(string),
				Provider: providerCognito,
			}

			if err := storage.Write(tx, hostedKey(scope, "browsersession", p.ID, cookie), sess); err != nil {
				return err
			}

			if oldSession != "" {
				tx.Delete(hostedKey(scope, "browsersession", p.ID, oldSession))
			}

			tx.Delete(hostedKey(scope, "hosted", p.ID, id))
			result.Session = cookie
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	if out != nil {
		out.messages.flush(s.deliveries)
	}

	if idp != nil && result.Redirect == "" {
		return s.beginFederation(ctx, poolID, id, pending, idp, result)
	}

	if g != nil {
		return s.finishAuthorization(ctx, result, g)
	}

	return result, nil
}

func hostedPool(tx storage.ReadTx, id string) (request.Scope, *pool, error) {
	account, ok := tx.Get(poolIndexKey(id))
	if !ok {
		return request.Scope{}, nil, notFound("User pool does not exist.")
	}

	region, _, _ := strings.Cut(id, "_")
	scope := request.Scope{AccountID: string(account), Region: region}
	p, err := loadPool(tx, scope, &id)
	return scope, p, err
}

func liveAuthorization(c *client, a *authorization) string {
	if !slices.Contains(c.Settings.CallbackURLs, a.Redirect) {
		return invalidRedirectURI
	}

	flow := a.Response
	if flow == responseToken {
		flow = flowImplicit
	}

	if !common.Value(c.Settings.AllowedOAuthFlowsUserPoolClient) ||
		!slices.Contains(c.Settings.AllowedOAuthFlows, flow) {
		return unauthorizedClient
	}

	for _, sc := range a.Scopes {
		if !slices.Contains(c.Settings.AllowedOAuthScopes, sc) {
			return invalidScope
		}
	}

	return ""
}

func (s *Service) completeAuthorization(
	tx storage.WriteTx, scope request.Scope, p *pool, a *authorization, g *grant,
) (*HostedResult, error) {
	applyOAuthGrant(g, a)
	r, ok, err := storage.Read[refresh](tx, refreshKey(scope, p.ID, g.refresh))
	if err != nil {
		return nil, err
	}

	if ok {
		r.OAuth, r.Scopes, r.Nonce = true, a.Scopes, a.Nonce
		r.AuthTime = time.Unix(g.access[claimAuthTime].(int64), 0)
		if err := storage.Write(tx, refreshKey(scope, p.ID, g.refresh), r); err != nil {
			return nil, err
		}
	}

	if a.Response == responseToken {
		tx.Delete(refreshKey(scope, p.ID, g.refresh))
		g.refresh = ""
		return &HostedResult{}, nil
	}

	id := common.RandomID(64, alphaNum)
	code := authorizationCode{
		Authorization: *a,
		Expires:       s.now().UTC().Add(5 * time.Minute),
		AuthTime:      time.Unix(g.access[claimAuthTime].(int64), 0),
		Username:      g.access[claimUsername].(string),
		Sub:           g.access[attrSub].(string),
		Origin:        g.access["origin_jti"].(string),
		Refresh:       g.refresh,
	}

	if err := storage.Write(tx, hostedKey(scope, "oauthcode", p.ID, id), code); err != nil {
		return nil, err
	}

	return &HostedResult{Redirect: authorizationRedirect(a, url.Values{flowCode: {id}})}, nil
}

func applyOAuthGrant(g *grant, a *authorization) {
	g.oauth = true
	g.authorization = a
	g.access[claimScope] = strings.Join(a.Scopes, " ")
	for name := range g.id {
		if slices.Contains([]string{"email", "email_verified", "phone_number", "phone_number_verified"}, name) ||
			slices.Contains(profileAttributes, name) || strings.HasPrefix(name, customPrefix) {
			if !scopedAttribute(name, a.Scopes) {
				delete(g.id, name)
			}
		}
	}

	if a.Nonce != "" {
		g.id["nonce"] = a.Nonce
	}

	if g.preToken != nil {
		g.preToken.event.TriggerSource = "TokenGeneration_" + sourceHostedAuth
		if g.preToken.event.Version != "1" {
			g.preToken.event.Request[triggerScopes] = a.Scopes
		}
	}
}

func (s *Service) finishAuthorization(ctx context.Context, result *HostedResult, g *grant) (*HostedResult, error) {
	if result.Redirect != "" {
		if err := s.invoke(ctx, g.postAuth, nil); err != nil {
			return nil, err
		}

		return result, nil
	}

	tokens, err := s.issue(ctx, g)
	if err != nil {
		return nil, err
	}

	// The implicit callback/state is attached by the caller through the grant's private redirect field.
	a := g.authorization
	result.Redirect = authorizationRedirect(a, url.Values{
		oauthAccessToken: {common.Value(tokens.AccessToken)},
		oauthTokenType:   {bearerTokenType}, oauthExpiresIn: {fmt.Sprint(common.Value(tokens.ExpiresIn))},
	})
	if tokens.IdToken != nil {
		u, _ := url.Parse(result.Redirect)
		v, _ := url.ParseQuery(u.Fragment)
		v.Set("id_token", *tokens.IdToken)
		u.Fragment = v.Encode()
		result.Redirect = u.String()
	}

	return result, nil
}

var profileAttributes = []string{
	attrName, attrFamilyName, attrGivenName, "middle_name", "nickname", "preferred_username",
	scopeProfile, "picture", "website", "gender", attrBirthdate, "zoneinfo", "locale", "updated_at", "address",
}

func scopedAttribute(name string, scopes []string) bool {
	if !slices.Contains(scopes, "email") && !slices.Contains(scopes, "phone") &&
		!slices.Contains(scopes, scopeProfile) {
		return true
	}

	switch name {
	case attrEmail, attrEmail + verifiedSuffix:
		return slices.Contains(scopes, attrEmail)
	case attrPhone, attrPhone + verifiedSuffix:
		return slices.Contains(scopes, "phone")
	default:
		return slices.Contains(scopes, scopeProfile)
	}
}
