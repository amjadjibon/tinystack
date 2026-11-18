package ssooidc

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/services/identitystore"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Prompt is what a sign-in page shows: the application asking, and the form's hidden value (a user code or an
// authorization transaction).
type Prompt struct {
	Client string
	Value  string
}

// signInError is a sign-in problem a page shows the user.
type signInError struct{ message string }

func (e *signInError) Error() string { return e.message }

// SignInMessage returns the message a page shows for err, or "" when err is not a sign-in problem.
func SignInMessage(err error) string {
	var e *signInError
	if errors.As(err, &e) {
		return e.message
	}

	return ""
}

func failSignIn(message string) error { return &signInError{message: message} }

func (s *Service) pendingDevice(tx storage.ReadTx, userCode string) (string, *device, error) {
	ref, err := read(tx, s.now(), kindUserCode, strings.ToUpper(strings.TrimSpace(userCode)),
		func(r *userCodeRef) int64 { return r.Expires })
	if err != nil {
		return "", nil, err
	}

	if ref == nil {
		return "", nil, failSignIn("This code is not valid or has expired. Start the sign-in again.")
	}

	d, err := read(tx, s.now(), kindDevice, ref.Device, func(d *device) int64 { return d.Expires })
	switch {
	case err != nil:
		return "", nil, err
	case d == nil:
		return "", nil, failSignIn("This code is not valid or has expired. Start the sign-in again.")
	case d.UserID != "":
		return "", nil, failSignIn("This sign-in has already been approved.")
	}

	return ref.Device, d, nil
}

// DevicePrompt describes a pending device sign-in for its approval page.
func (s *Service) DevicePrompt(ctx context.Context, userCode string) (*Prompt, error) {
	var out *Prompt
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		_, d, err := s.pendingDevice(tx, userCode)
		if err != nil {
			return err
		}

		c, _, err := storage.Read[client](tx, key(kindClient, d.ClientID))
		switch {
		case err != nil:
			return err
		case c == nil:
			return failSignIn("The application is no longer registered.")
		}

		out = &Prompt{Client: c.Name, Value: d.UserCode}
		return nil
	})

	return out, err
}

// ApproveDevice signs the named Identity Store user in to a pending device sign-in.
func (s *Service) ApproveDevice(ctx context.Context, userCode, userName string) error {
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		hashed, d, err := s.pendingDevice(tx, userCode)
		if err != nil {
			return err
		}

		u, found, err := identitystore.UserByName(tx, d.Account, strings.TrimSpace(userName))
		switch {
		case err != nil:
			return err
		case !found:
			return failSignIn("No user has that user name.")
		}

		d.UserID = *u.UserId
		return storage.Write(tx, key(kindDevice, hashed), d)
	})
}

// Authorize starts an authorization code sign-in from the query of a request to /authorize. Requests that name no
// valid client and redirect URI fail on the page, never by redirecting.
func (s *Service) Authorize(ctx context.Context, query url.Values) (*Prompt, error) {
	if query.Get("response_type") != "code" {
		return nil, failSignIn("response_type must be code.")
	}

	if query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" {
		return nil, failSignIn("A PKCE code challenge with method S256 is required.")
	}

	now := s.now()
	transaction := random(24)
	var out *Prompt
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		if err := prune(tx, now); err != nil {
			return err
		}

		c, err := read(tx, now, kindClient, query.Get("client_id"), func(c *client) int64 { return c.Expires })
		switch {
		case err != nil:
			return err
		case c == nil || !c.allows(grantCode):
			return failSignIn("The application is not registered for this sign-in.")
		case !slices.Contains(c.RedirectURIs, query.Get("redirect_uri")):
			return failSignIn("The redirect URI is not registered for this application.")
		}

		requested := scopes(query)
		for _, scope := range requested {
			if !slices.Contains(c.Scopes, scope) {
				return failSignIn("The application did not register the scope " + scope + ".")
			}
		}

		account, found := identitystore.StartURLAccount(tx, c.IssuerURL)
		if !found {
			return failSignIn("The application's issuer URL is not a known access portal start URL.")
		}

		out = &Prompt{Client: c.Name, Value: transaction}
		return storage.Write(tx, key(kindAuthorization, transaction), &authorization{
			ClientID: query.Get("client_id"), RedirectURI: query.Get("redirect_uri"), State: query.Get("state"),
			Challenge: query.Get("code_challenge"), Account: account, Scopes: requested,
			Expires: now.Add(deviceLifetime).Unix(),
		})
	})

	return out, err
}

// ApproveAuthorization signs the named user in to a pending authorization code sign-in and returns the redirect
// that hands the client its code.
func (s *Service) ApproveAuthorization(ctx context.Context, transaction, userName string) (string, error) {
	now := s.now()
	authorizationCode := random(32)
	var redirect string
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, err := read(tx, now, kindAuthorization, transaction, func(a *authorization) int64 { return a.Expires })
		switch {
		case err != nil:
			return err
		case a == nil:
			return failSignIn("This sign-in has expired. Start it again from the application.")
		}

		u, found, err := identitystore.UserByName(tx, a.Account, strings.TrimSpace(userName))
		switch {
		case err != nil:
			return err
		case !found:
			return failSignIn("No user has that user name.")
		}

		target, err := url.Parse(a.RedirectURI)
		if err != nil {
			return err
		}

		q := target.Query()
		q.Set("code", authorizationCode)
		if a.State != "" {
			q.Set("state", a.State)
		}

		target.RawQuery = q.Encode()
		redirect = target.String()
		tx.Delete(key(kindAuthorization, transaction))
		return storage.Write(tx, key(kindCode, hash(authorizationCode)), &code{
			ClientID: a.ClientID, RedirectURI: a.RedirectURI, Challenge: a.Challenge, Account: a.Account,
			UserID: *u.UserId, Scopes: a.Scopes, Expires: now.Add(codeLifetime).Unix(),
		})
	})

	return redirect, err
}
