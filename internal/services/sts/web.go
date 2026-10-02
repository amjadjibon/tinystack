package sts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sts"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const webIdentityAction = "sts:AssumeRoleWithWebIdentity"

// claims are the parts of a web identity token that STS reads.
type claims struct {
	Issuer   string          `json:"iss"`
	Subject  string          `json:"sub"`
	Audience json.RawMessage `json:"aud"`
	Expires  float64         `json:"exp"`
}

func invalidToken(message string) error {
	return awserror.New("InvalidIdentityToken", message)
}

// parseToken decodes a JWT's claims. TinyStack does not verify its signature:
// it trusts any token whose issuer is a provider of the role's account.
func parseToken(token string) (*claims, []string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, nil, invalidToken("The ID Token provided is not a valid JWT")
	}

	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, nil, invalidToken("The ID Token provided is not a valid JWT")
	}

	var c claims
	if err = json.Unmarshal(payload, &c); err != nil || c.Issuer == "" || c.Subject == "" || c.Expires == 0 {
		return nil, nil, invalidToken("The ID Token provided is not a valid JWT")
	}

	var audiences []string
	if err = json.Unmarshal(c.Audience, &audiences); err != nil {
		var one string
		if json.Unmarshal(c.Audience, &one) != nil || one == "" {
			return nil, nil, invalidToken("The ID Token provided is not a valid JWT")
		}

		audiences = []string{one}
	}

	return &c, audiences, nil
}

func checkWebIdentity(in *api.AssumeRoleWithWebIdentityRequest) error {
	err := checkAssumeRole(&api.AssumeRoleRequest{RoleArn: in.RoleArn, RoleSessionName: in.RoleSessionName})
	if err != nil {
		return err
	}

	if n := len(value(in.WebIdentityToken)); n < 4 || n > 20000 {
		return validation("1 validation error detected: Value at 'webIdentityToken' failed to satisfy constraint: " +
			"Member must have length between 4 and 20000")
	}

	return nil
}

// AssumeRoleWithWebIdentity issues temporary credentials for a role whose
// trust policy names the OpenID Connect provider a token came from. The
// provider must be one of the role's account's, and one of its client IDs the
// token's audience; the trust policy's conditions can name the provider's
// "<URL>:aud" and "<URL>:sub" keys. The request needs no signature.
func (s *Service) AssumeRoleWithWebIdentity(
	ctx context.Context,
	in *api.AssumeRoleWithWebIdentityRequest,
) (*api.AssumeRoleWithWebIdentityResponse, error) {
	if err := checkWebIdentity(in); err != nil {
		return nil, err
	}

	token, audiences, err := parseToken(*in.WebIdentityToken)
	if err != nil {
		return nil, err
	}

	now := s.now()
	if expires := time.Unix(int64(token.Expires), 0); !now.Before(expires) {
		return nil, awserror.New("ExpiredTokenException", fmt.Sprintf("Token expired: current date/time %d must "+
			"be before the expiration date/time %d", now.Unix(), expires.Unix()))
	}

	role, exists, err := s.roles.RoleByARN(ctx, *in.RoleArn)
	if err != nil {
		return nil, err
	}

	notAuthorized := denied("Not authorized to perform " + webIdentityAction)
	if !exists {
		return nil, notAuthorized
	}

	provider, found, err := s.roles.OIDCProvider(ctx, role.Account, token.Issuer)
	switch {
	case err != nil:
		return nil, err
	case !found:
		return nil, invalidToken("No OpenIDConnect provider found in your account for " + token.Issuer)
	}

	audience := ""
	for _, a := range audiences {
		if slices.Contains(provider.ClientIDs, a) {
			audience = a
		}
	}

	if audience == "" {
		return nil, invalidToken("Incorrect token audience")
	}

	url, _ := strings.CutPrefix(provider.ARN, "arn:aws:iam::"+role.Account+":oidc-provider/")
	c := caller{
		account: role.Account, federated: provider.ARN, action: webIdentityAction,
		context: map[string]string{url + ":aud": audience, url + ":sub": token.Subject},
	}
	if !trusts(role.TrustPolicy, &c) {
		return nil, notAuthorized
	}

	duration, err := sessionDuration(in.DurationSeconds, role.MaxSessionDuration, false)
	if err != nil {
		return nil, err
	}

	var out *api.AssumeRoleWithWebIdentityResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		issued := &session{
			AccountID: role.Account, RoleARN: role.ARN, RoleName: role.Name, RoleID: role.ID,
			Name: *in.RoleSessionName, Expires: now.Add(duration).Unix(),
		}
		credentials, issueErr := issue(tx, issued, now)
		if issueErr != nil {
			return issueErr
		}

		out = &api.AssumeRoleWithWebIdentityResponse{
			Credentials:                 credentials,
			AssumedRoleUser:             &api.AssumedRoleUser{AssumedRoleId: new(issued.id()), Arn: new(issued.arn())},
			SubjectFromWebIdentityToken: new(token.Subject), Provider: new(token.Issuer), Audience: new(audience),
		}

		return nil
	})

	return out, err
}

// UseAccounts lets GetAccessKeyInfo answer for the configured access keys,
// which belong to their configured accounts or else to fallback. Call it
// before serving requests.
func (s *Service) UseAccounts(accounts map[string]string, fallback string) {
	s.accounts, s.fallback = accounts, fallback
}

// GetAccessKeyInfo returns the account an access key belongs to: a session's,
// an IAM user's, or a configured key's.
func (s *Service) GetAccessKeyInfo(
	ctx context.Context,
	in *api.GetAccessKeyInfoRequest,
) (*api.GetAccessKeyInfoResponse, error) {
	id := value(in.AccessKeyId)
	if len(id) < 16 || len(id) > 128 {
		return nil, validation("1 validation error detected: Value at 'accessKeyId' failed to satisfy constraint: " +
			"Member must have length between 16 and 128")
	}

	credentials, found, err := s.Credentials(ctx, id)
	switch {
	case err != nil:
		return nil, err
	case found:
		return &api.GetAccessKeyInfoResponse{Account: new(credentials.AccountID)}, nil
	}

	account, ok := s.accounts[id]
	if !ok {
		account = s.fallback
	}

	return &api.GetAccessKeyInfoResponse{Account: new(account)}, nil
}
