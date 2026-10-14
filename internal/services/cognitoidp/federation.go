package cognitoidp

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// AWS accepts RSA, ECDSA and HMAC-SHA OIDC signatures; go-oidc verifies them through the provider JWKS.
var oidcSigningAlgorithms = []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "HS256", "HS384", "HS512"}

// federationTransaction binds the provider and callback to the original client, browser and authorization request.
type federationTransaction struct {
	Provider    api.IdentityProviderType
	Expires     time.Time
	Transaction string
	StateHash   string
	Nonce       string
	Verifier    string
	Callback    string
	Hosted      hostedTransaction
}

type providerDiscovery struct {
	OAuth    oauth2.Config
	Verifier *oidc.IDTokenVerifier
	UserInfo string
}

// Discovery and token exchange use a bounded context and never happen inside storage transactions.
func discoverProvider(ctx context.Context, idp *api.IdentityProviderType, callback string) (*providerDiscovery, error) {
	details := idp.ProviderDetails
	out := &providerDiscovery{OAuth: oauth2.Config{
		ClientID: details[claimClientID], ClientSecret: details["client_secret"],
		RedirectURL: callback, Scopes: strings.Fields(details["authorize_scopes"]), Endpoint: oauth2.Endpoint{
			AuthURL: details["authorize_url"], TokenURL: details["token_url"], AuthStyle: oauth2.AuthStyleInParams,
		},
	}, UserInfo: details["attributes_url"]}
	complete := out.OAuth.Endpoint.AuthURL != "" && out.OAuth.Endpoint.TokenURL != "" && details[providerJWKS] != ""
	if complete {
		out.Verifier = oidc.NewVerifier(details["oidc_issuer"], oidc.NewRemoteKeySet(ctx, details[providerJWKS]),
			&oidc.Config{ClientID: details[claimClientID], SupportedSigningAlgs: oidcSigningAlgorithms})
		return out, nil
	}

	provider, err := oidc.NewProvider(ctx, details["oidc_issuer"])
	if err != nil {
		return nil, errors.New("OIDC discovery failed")
	}

	endpoints := provider.Endpoint()
	if out.OAuth.Endpoint.AuthURL == "" {
		out.OAuth.Endpoint.AuthURL = endpoints.AuthURL
	}

	if out.OAuth.Endpoint.TokenURL == "" {
		out.OAuth.Endpoint.TokenURL = endpoints.TokenURL
	}

	if details[providerJWKS] != "" {
		out.Verifier = oidc.NewVerifier(details["oidc_issuer"], oidc.NewRemoteKeySet(ctx, details[providerJWKS]),
			&oidc.Config{ClientID: details[claimClientID], SupportedSigningAlgs: oidcSigningAlgorithms})
	} else {
		out.Verifier = provider.Verifier(
			&oidc.Config{ClientID: details[claimClientID], SupportedSigningAlgs: oidcSigningAlgorithms},
		)
	}

	if out.UserInfo == "" {
		var doc struct {
			UserInfo string `json:"userinfo_endpoint"`
		}

		if err := provider.Claims(&doc); err != nil {
			return nil, err
		}

		out.UserInfo = doc.UserInfo
	}

	for _, endpoint := range []string{out.OAuth.Endpoint.AuthURL, out.OAuth.Endpoint.TokenURL, out.UserInfo} {
		if endpoint != "" && !validOIDCEndpoint(endpoint) {
			return nil, errors.New("OIDC discovery returned an invalid endpoint")
		}
	}

	return out, nil
}

func federationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	client := &http.Client{Timeout: 10 * time.Second}
	return context.WithTimeout(oidc.ClientContext(ctx, client), 10*time.Second)
}

func (s *Service) beginFederation(
	ctx context.Context,
	poolID, id string,
	pending *hostedTransaction,
	idp *api.IdentityProviderType,
	result *HostedResult,
) (*HostedResult, error) {
	metadata, _ := request.FromContext(ctx)
	callback := metadata.Endpoint + "/" + poolID + "/oauth2/idpresponse"
	parsed, _ := url.Parse(metadata.Endpoint)
	if _, found, operationErr := s.HostedPool(ctx, parsed.Host); operationErr != nil {
		return nil, operationErr
	} else if found {
		callback = metadata.Endpoint + "/oauth2/idpresponse"
	}

	discoveryCtx, cancel := federationContext(ctx)
	defer cancel()
	discovery, operationErr := discoverProvider(discoveryCtx, idp, callback)
	if operationErr != nil {
		return s.federationFailure(
			ctx,
			poolID,
			&pending.Authorization,
			"server_error",
			"Unable to discover OIDC provider.",
		)
	}

	state := common.RandomID(64, alphaNum)
	transaction := federationTransaction{
		Hosted: *pending, Provider: *idp, Expires: pending.Expires,
		Transaction: id, StateHash: opaqueHash(state), Nonce: common.RandomID(64, alphaNum),
		Verifier: oauth2.GenerateVerifier(), Callback: callback,
	}

	operationErr = s.store.Update(ctx, func(tx storage.WriteTx) error {
		scope, p, err := hostedPool(tx, poolID)
		if err != nil {
			return err
		}

		current, ok, err := storage.Read[hostedTransaction](tx, hostedKey(scope, "hosted", p.ID, id))
		if err != nil {
			return err
		}

		if !ok || !reflect.DeepEqual(current, pending) || !s.now().Before(current.Expires) {
			return invalidParameter("Sign-in transaction changed or expired.")
		}

		return storage.Write(tx, hostedKey(scope, "idptx", p.ID, state), transaction)
	})
	if operationErr != nil {
		return nil, operationErr
	}

	options := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(transaction.Verifier), oidc.Nonce(transaction.Nonce)}
	if pending.Authorization.Prompt != "" && pending.Authorization.Prompt != promptNone {
		options = append(options, oauth2.SetAuthURLParam("prompt", pending.Authorization.Prompt))
	}

	if pending.Authorization.Hint != "" {
		options = append(options, oauth2.SetAuthURLParam("login_hint", pending.Authorization.Hint))
	}

	result.Redirect = discovery.OAuth.AuthCodeURL(state, options...)
	return result, nil
}

// IdPResponse consumes state before contacting the provider and verifies the complete OIDC response.
func (s *Service) IdPResponse(
	ctx context.Context, poolID string, values url.Values, browser, oldSession string,
) (*HostedResult, error) {
	state := values.Get("state")
	var transaction *federationTransaction
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		scope, p, err := hostedPool(tx, poolID)
		if err != nil {
			return err
		}

		var ok bool
		transaction, ok, err = storage.Read[federationTransaction](tx, hostedKey(scope, "idptx", p.ID, state))
		if err != nil {
			return err
		}

		if !ok || state == "" || !s.now().Before(transaction.Expires) || browser == "" ||
			!hmac.Equal([]byte(transaction.StateHash), []byte(opaqueHash(state))) ||
			!hmac.Equal([]byte(transaction.Hosted.Browser), []byte(opaqueHash(browser))) {
			return invalidParameter("Invalid or expired OIDC state.")
		}

		tx.Delete(hostedKey(scope, "idptx", p.ID, state))
		return nil
	})
	if err != nil {
		return nil, err
	}

	a := &transaction.Hosted.Authorization
	if values.Get("error") != "" || values.Get(flowCode) == "" {
		return s.federationFailure(ctx, poolID, a, "access_denied", "OIDC provider refused sign-in.")
	}

	exchangeCtx, cancel := federationContext(ctx)
	defer cancel()
	discovery, err := discoverProvider(exchangeCtx, &transaction.Provider, transaction.Callback)
	if err != nil {
		return s.federationFailure(ctx, poolID, a, "server_error", "OIDC discovery failed.")
	}

	exchanged, err := discovery.OAuth.Exchange(
		exchangeCtx,
		values.Get(flowCode),
		oauth2.VerifierOption(transaction.Verifier),
	)
	if err != nil {
		return s.federationFailure(ctx, poolID, a, invalidRequest, "OIDC code exchange failed.")
	}

	raw, _ := exchanged.Extra("id_token").(string)
	idToken, err := discovery.Verifier.Verify(exchangeCtx, raw)
	if err != nil || !hmac.Equal([]byte(idToken.Nonce), []byte(transaction.Nonce)) || idToken.Subject == "" {
		return s.federationFailure(ctx, poolID, a, invalidRequest, "OIDC ID token could not be verified.")
	}

	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return nil, err
	}

	if discovery.UserInfo != "" {
		attributes, err := providerUserInfo(exchangeCtx, discovery.UserInfo,
			transaction.Provider.ProviderDetails["attributes_request_method"], exchanged.AccessToken)
		if err != nil || attributes[attrSub] != idToken.Subject {
			return s.federationFailure(ctx, poolID, a, invalidRequest, "OIDC userInfo subject could not be verified.")
		}

		for k, v := range attributes {
			claims[k] = v
		}
	}

	return s.federatedSignIn(ctx, poolID, transaction, claims, oldSession)
}

func providerUserInfo(ctx context.Context, endpoint, method, access string) (map[string]any, error) {
	if method == "" {
		method = http.MethodGet
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, http.NoBody)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+access)
	client := oauth2.NewClient(ctx, nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("OIDC userInfo failed")
	}

	var claims map[string]any
	err = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&claims)
	return claims, err
}

type federatedIdentity struct {
	UserID       string `json:"userId"`
	ProviderName string `json:"providerName"`
	ProviderType string `json:"providerType"`
	Issuer       string `json:"issuer"`
	Primary      bool   `json:"primary"`
	DateCreated  int64  `json:"dateCreated"`
}

func (s *Service) federatedSignIn(
	ctx context.Context, poolID string, transaction *federationTransaction, claims map[string]any, oldSession string,
) (*HostedResult, error) {
	var result *HostedResult
	var g *grant
	var confirmed *call
	sub := newSub()
	created := s.now().UTC()
	a := &transaction.Hosted.Authorization
	err := s.runAuth(ctx, func(ctx context.Context) error {
		return s.store.Update(ctx, func(tx storage.WriteTx) error {
			scope, p, err := hostedPool(tx, poolID)
			if err != nil {
				return err
			}

			pending, ok, err := storage.Read[hostedTransaction](
				tx,
				hostedKey(scope, "hosted", p.ID, transaction.Transaction),
			)
			if err != nil {
				return err
			}

			if !ok || !s.now().Before(pending.Expires) || !reflect.DeepEqual(pending, &transaction.Hosted) {
				return invalidParameter("OIDC sign-in transaction changed or expired.")
			}

			idp, err := loadProvider(tx, scope, p, transaction.Provider.ProviderName)
			if err != nil {
				return err
			}

			if !reflect.DeepEqual(idp, &transaction.Provider) {
				return invalidParameter("OIDC provider changed during sign-in.")
			}

			c, err := loadClient(tx, scope, p, &a.Client)
			if err != nil {
				return err
			}

			if !providerAllowed(c, common.Value(idp.ProviderName)) || liveAuthorization(c, a) != "" {
				return invalidParameter("Client settings changed during sign-in.")
			}

			subject, _ := claims[attrSub].(string)
			name := common.Value(idp.ProviderName) + "_" + subject
			if p.caseInsensitive() {
				name = strings.ToLower(name)
			}

			if nameErr := checkUsername(&name); nameErr != nil {
				return nameErr
			}

			u, exists, err := storage.Read[user](tx, userKey(scope, p, name))
			if err != nil {
				return err
			}

			identity := federatedIdentity{
				UserID: subject, ProviderName: common.Value(idp.ProviderName), ProviderType: providerOIDC,
				Issuer: idp.ProviderDetails["oidc_issuer"], Primary: true, DateCreated: created.UnixMilli(),
			}

			var before *user
			if exists {
				var identities []federatedIdentity
				if json.Unmarshal([]byte(u.Attributes[attrIdentities]), &identities) != nil ||
					!slices.ContainsFunc(identities,
						func(i federatedIdentity) bool {
							return i.UserID == subject && i.ProviderName == identity.ProviderName &&
								i.Issuer == identity.Issuer
						}) {
					return notAuthorized("Federated username belongs to another identity.")
				}

				before = common.Clone(u)
			} else {
				u = &user{
					Username: name, Attributes: map[string]string{attrSub: sub}, Status: "EXTERNAL_PROVIDER",
					Created: created, Modified: created,
				}

				identities, marshalErr := json.Marshal([]federatedIdentity{identity})
				if marshalErr != nil {
					return marshalErr
				}

				u.Attributes[attrIdentities] = string(identities)
			}

			if u.Disabled {
				return notAuthorized("User is disabled.")
			}

			attributes := []api.AttributeType{}
			for attr, source := range idp.AttributeMapping {
				if value, ok := claims[source]; ok {
					if !writable(c, strings.TrimSuffix(attr, verifiedSuffix)) {
						return invalidParameter("Mapped attribute %s is not writable by this client.", attr)
					}

					text := fmt.Sprint(value)
					if structured, ok := value.([]any); ok {
						encoded, encodeErr := json.Marshal(structured)
						if encodeErr != nil {
							return encodeErr
						}

						text = string(encoded)
					}

					attributes = append(attributes, api.AttributeType{Name: new(attr), Value: new(text)})
				}
			}

			if _, attributeErr := setAttributes(p, u, attributes, !exists); attributeErr != nil {
				return attributeErr
			}

			if requiredErr := checkRequired(p, u); requiredErr != nil {
				return requiredErr
			}

			if !exists {
				if function := common.Value(p.triggers().PreSignUp); function != "" {
					call := newCall(scope, p, c.ID, function, "PreSignUp", "PreSignUp_ExternalProvider", u, nil)
					delete(call.event.Request["userAttributes"].(map[string]string), attrUserStatus)
					var response preSignUp
					if triggerErr := customInvoke(ctx, call, &response); triggerErr != nil {
						return triggerErr
					}

					response.applyTo(u)
					u.Status = "EXTERNAL_PROVIDER"
				}

				confirmed = postConfirmation(scope, p, c.ID, "PostConfirmation_ConfirmSignUp", u, nil)
			}

			if saveErr := saveUser(tx, scope, p, before, u, false); saveErr != nil {
				return saveErr
			}

			g, err = s.signIn(ctx, tx, scope, p, c, u, sourceAuth)
			if err != nil {
				return err
			}

			result, err = s.completeAuthorization(tx, scope, p, a, g)
			if err != nil {
				return err
			}

			cookie := common.RandomID(64, alphaNum)
			sess := browserSession{
				Expires: s.now().UTC().Add(hostedCookieTTL), AuthTime: s.now().UTC(), Username: u.Username,
				Sub: u.Attributes[attrSub], Origin: g.access["origin_jti"].(string), Provider: identity.ProviderName,
			}

			if err := storage.Write(tx, hostedKey(scope, "browsersession", p.ID, cookie), sess); err != nil {
				return err
			}

			tx.Delete(hostedKey(scope, "hosted", p.ID, transaction.Transaction))
			if oldSession != "" {
				tx.Delete(hostedKey(scope, "browsersession", p.ID, oldSession))
			}

			result.Session = cookie
			return nil
		})
	})
	if err != nil {
		return s.federationFailure(ctx, poolID, a, invalidRequest, "Federated user could not be signed in.")
	}

	if err := s.invoke(ctx, confirmed, nil); err != nil {
		return nil, err
	}

	return s.finishAuthorization(ctx, result, g)
}

// federationFailure rechecks the callback after external I/O, so withdrawing a callback or deleting
// the app client never turns an IdP error into a redirect to a now-unregistered URI.
func (s *Service) federationFailure(
	ctx context.Context, poolID string, a *authorization, code, message string,
) (*HostedResult, error) {
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		scope, p, err := hostedPool(tx, poolID)
		if err != nil {
			return err
		}

		c, err := loadClient(tx, scope, p, &a.Client)
		if err != nil {
			return err
		}

		if !slices.Contains(c.Settings.CallbackURLs, a.Redirect) {
			return invalidParameter("The callback is no longer allowed.")
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return authorizationFailure(a, code, message), nil
}
