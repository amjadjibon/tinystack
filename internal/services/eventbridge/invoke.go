package eventbridge

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// A rule target whose ARN is an API destination makes a real HTTP request: the destination's method to its
// endpoint, with the rendered event as the body, the target's HttpParameters, and the connection's authorization.
// PutEvents waits for it, up to invokeTimeout. A response other than 2xx fails the delivery, which is retried
// (retry.go) when a later attempt can succeed.

const (
	// invokeTimeout bounds one request, as in AWS.
	invokeTimeout = 5 * time.Second
	// maxResponse is how much of a response is read; the rest is dropped.
	maxResponse = 1 << 20
	// defaultTokenTTL is how long an access token that states no lifetime is kept.
	defaultTokenTTL = time.Hour
	// codeRetryable and codeFailed are the error codes of a failed request: a later attempt may get past the first
	// kind, such as a server error, and not the second, such as a missing connection or a 400 response.
	codeRetryable = "ApiDestinationRetryable"
	codeFailed    = "ApiDestinationFailed"
)

// retryableStatuses are the 4xx responses that, like every 5xx one, are retried, as in AWS.
var retryableStatuses = map[int]bool{
	http.StatusUnauthorized:      true,
	http.StatusProxyAuthRequired: true,
	http.StatusConflict:          true,
	http.StatusTooManyRequests:   true,
}

// newClient returns the client that makes every API destination request. It does not follow redirects, which
// fail the delivery.
func newClient() *http.Client {
	return &http.Client{
		Timeout:       invokeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// failure is an error no later attempt can fix.
func failure(format string, args ...any) error {
	return awserror.New(codeFailed, fmt.Sprintf(format, args...))
}

// transportError reports a request that got no response, without the URL, whose query string may hold secrets.
func transportError(what string, err error) error {
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		err = urlErr.Err
	}

	return awserror.New(codeRetryable, "The request to the "+what+" failed: "+err.Error())
}

// statusError reports a response other than 2xx.
func statusError(what string, status int) error {
	message := fmt.Sprintf("The %s responded with HTTP status %d.", what, status)
	if status >= 500 || retryableStatuses[status] {
		return awserror.New(codeRetryable, message)
	}

	return awserror.New(codeFailed, message)
}

// do makes a request and returns the response body when the status is 2xx. It returns the status of any response.
func (s *Service) do(req *http.Request, what string) (body []byte, status int, err error) {
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, transportError(what, err)
	}

	defer func() { _ = resp.Body.Close() }()
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, resp.StatusCode, transportError(what, err)
	}

	if resp.StatusCode/100 != 2 {
		return nil, resp.StatusCode, statusError(what, resp.StatusCode)
	}

	return body, resp.StatusCode, nil
}

// invoke delivers payload to the API destination t names.
func (s *Service) invoke(ctx context.Context, t *target, payload []byte) error {
	m, err := request.Regional(ctx)
	if err != nil {
		return err
	}

	var d *destination
	var c *connection
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		if d, loadErr = destinationByARN(tx, m.Scope, t.ARN); loadErr != nil {
			return loadErr
		}

		c, loadErr = connectionByARN(tx, m.Scope, d.ConnectionARN)
		return loadErr
	})
	if err != nil {
		return err
	}

	if c.State != connectionAuthorized {
		return failure("The connection %s of the API destination is %s.", c.Name, c.State)
	}

	req, err := s.newRequest(ctx, m.Scope, d, c, t, payload)
	if err != nil {
		return err
	}

	_, status, err := s.do(req, "API destination "+d.Name)
	if status == http.StatusUnauthorized && c.Type == authOAuth {
		s.tokens.drop(c.arn(m.Scope))
	}

	return err
}

// newRequest builds the request for one delivery. Later sources win where two set the same query string
// parameter or header: the endpoint's, the target's, the connection's, and its authorization's.
func (s *Service) newRequest(
	ctx context.Context,
	scope request.Scope,
	d *destination,
	c *connection,
	t *target,
	payload []byte,
) (*http.Request, error) {
	u, err := url.Parse(d.Endpoint)
	if err != nil {
		return nil, failure("The endpoint of the API destination is not valid.")
	}

	headers := http.Header{"Content-Type": {"application/json"}}
	query := u.Query()
	if h := t.HTTP; h != nil {
		if len(h.Paths) > 0 {
			if u.Path, err = fillPath(u.Path, h.Paths); err != nil {
				return nil, err
			}

			u.RawPath = ""
		}

		for k, v := range h.Headers {
			headers.Set(k, v)
		}

		for k, v := range h.Query {
			query.Set(k, v)
		}
	}

	for _, p := range c.Parameters.Headers {
		headers.Set(p.Key, p.Value)
	}

	for _, p := range c.Parameters.Query {
		query.Set(p.Key, p.Value)
	}

	u.RawQuery = query.Encode()
	if payload, err = withBodyParameters(payload, c.Parameters.Body); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, d.Method, u.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, failure("The request to the API destination is not valid: %s", err)
	}

	req.Header = headers
	switch c.Type {
	case authAPIKey:
		req.Header.Set(c.KeyName, c.KeyValue)
	case authBasic:
		req.SetBasicAuth(c.Username, c.Password)
	case authOAuth:
		token, tokenErr := s.accessToken(ctx, scope, c)
		if tokenErr != nil {
			return nil, tokenErr
		}

		req.Header.Set("Authorization", token)
	}

	return req, nil
}

// fillPath puts the values of PathParameterValues in place of the "*" of a path, in order.
func fillPath(path string, values []string) (string, error) {
	parts := strings.Split(path, "*")
	if len(parts) != len(values)+1 {
		return "", failure("The endpoint path has %d * wildcards and the target gives %d PathParameterValues.",
			len(parts)-1, len(values))
	}

	var b strings.Builder
	for i, part := range parts {
		b.WriteString(part)
		if i < len(values) {
			b.WriteString(values[i])
		}
	}

	return b.String(), nil
}

// withBodyParameters adds the connection's body parameters to the event, which must then be a JSON object.
func withBodyParameters(payload []byte, params []httpParameter) ([]byte, error) {
	if len(params) == 0 {
		return payload, nil
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(payload, &body); err != nil || body == nil {
		return nil, failure("The connection has body parameters, so the event sent must be a JSON object.")
	}

	for _, p := range params {
		body[p.Key], _ = json.Marshal(p.Value)
	}

	return json.Marshal(body)
}

// accessToken returns the Authorization header value of an OAuth connection: a cached access token, or a new one
// from the authorization endpoint, which is kept until it expires.
func (s *Service) accessToken(ctx context.Context, scope request.Scope, c *connection) (string, error) {
	key := c.arn(scope)
	if token, ok := s.tokens.get(key, s.now()); ok {
		return token, nil
	}

	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return "", failure("The authorization endpoint of the connection is not valid.")
	}

	query, form := u.Query(), url.Values{"grant_type": {"client_credentials"}}
	for _, p := range c.OAuthParameters.Query {
		query.Set(p.Key, p.Value)
	}

	for _, p := range c.OAuthParameters.Body {
		form.Set(p.Key, p.Value)
	}

	var body io.Reader
	if c.Method == http.MethodGet {
		for k, v := range form {
			query[k] = v
		}
	} else {
		body = strings.NewReader(form.Encode())
	}

	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, c.Method, u.String(), body)
	if err != nil {
		return "", failure("The request to the authorization endpoint is not valid: %s", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	req.SetBasicAuth(c.ClientID, c.ClientSecret)
	for _, p := range c.OAuthParameters.Headers {
		req.Header.Set(p.Key, p.Value)
	}

	data, _, err := s.do(req, "authorization endpoint of connection "+c.Name)
	if err != nil {
		return "", err
	}

	var response struct {
		AccessToken string      `json:"access_token"`
		TokenType   string      `json:"token_type"`
		ExpiresIn   json.Number `json:"expires_in"`
	}
	if jsonErr := json.Unmarshal(data, &response); jsonErr != nil || response.AccessToken == "" {
		return "", failure("The authorization endpoint of connection %s returned no access_token.", c.Name)
	}

	token := cmp.Or(response.TokenType, "Bearer") + " " + response.AccessToken
	ttl := defaultTokenTTL
	if response.ExpiresIn != "" {
		if seconds, numErr := response.ExpiresIn.Int64(); numErr == nil {
			ttl = time.Duration(seconds) * time.Second
		}
	}

	if ttl > 0 {
		s.tokens.put(key, token, s.now().Add(ttl))
	}

	return token, nil
}

// tokenCache keeps the access token of each OAuth connection, by connection ARN, until it expires.
type tokenCache struct {
	tokens map[string]cachedToken
	mu     sync.Mutex
}

type cachedToken struct {
	expires time.Time
	value   string
}

func (c *tokenCache) get(key string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tokens[key]
	return t.value, ok && now.Before(t.expires)
}

func (c *tokenCache) put(key, value string, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens == nil {
		c.tokens = map[string]cachedToken{}
	}

	c.tokens[key] = cachedToken{value: value, expires: expires}
}

// drop forgets a connection's token, such as when its credentials change or the destination rejects it.
func (c *tokenCache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.tokens, key)
}
