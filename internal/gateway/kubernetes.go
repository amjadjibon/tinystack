package gateway

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/aws/sigv4"
	"github.com/amjadjibon/tinystack/internal/config"
)

const (
	// tokenPrefix starts the bearer tokens "aws eks get-token" makes: a
	// presigned STS GetCallerIdentity URL that also signs x-k8s-aws-id.
	tokenPrefix = "k8s-aws-v1." // #nosec G101 -- a token format prefix, not a credential.
	// tokenLifetime is how long EKS accepts a token after it was signed,
	// whatever its X-Amz-Expires says.
	tokenLifetime = 15 * time.Minute
	// emptyHash is the SHA-256 of an empty payload.
	emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// ErrKubernetesToken is KubernetesAccount's error for a request without a
// valid EKS token.
var ErrKubernetesToken = errors.New("not a valid EKS token")

// KubernetesAccount returns the account of the access key that signed a
// Kubernetes API request's bearer token for cluster, resolving keys as SigV4
// requests do. In identity.mode verify the token's signature, cluster, and age
// are checked as EKS checks them.
func (s *Server) KubernetesAccount(r *http.Request, cluster string) (string, error) {
	encoded, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "+tokenPrefix)
	if !ok {
		return "", ErrKubernetesToken
	}

	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(encoded, "="))
	if err != nil {
		return "", ErrKubernetesToken
	}

	presigned, err := url.Parse(string(raw))
	if err != nil || presigned.Query().Get("Action") != opGetCallerIdentity {
		return "", ErrKubernetesToken
	}

	a, err := sigv4.ParseQuery(presigned.Query(), 0)
	if err != nil || !slices.Contains(a.SignedHeaders, "x-k8s-aws-id") {
		return "", ErrKubernetesToken
	}

	account := s.cfg.Defaults.Account
	secret, configured := s.cfg.Identity.SecretKeys[a.AccessKey]
	var issued *request.Credentials
	digits, mapped, named := s.keyAccount(a.AccessKey)
	if named && mapped {
		account = digits
	} else {
		if s.credentials != nil && !configured {
			credentials, found, lookupErr := s.credentials(r.Context(), a.AccessKey)
			if lookupErr != nil {
				return "", lookupErr
			}

			if found {
				account, issued, secret = credentials.AccountID, &credentials, credentials.Secret
			}
		}

		if issued == nil && named {
			account = digits
		}
	}

	if s.cfg.Identity.Mode != config.IdentityVerify {
		return account, nil
	}

	token := presigned.Query().Get(sigv4.QuerySecurityToken)
	switch {
	case !configured && issued == nil:
		return "", ErrKubernetesToken
	case issued != nil && subtle.ConstantTimeCompare([]byte(token), []byte(issued.Token)) != 1:
		return "", ErrKubernetesToken
	case issued != nil && !issued.Expires.IsZero() && !time.Now().Before(issued.Expires):
		return "", ErrKubernetesToken
	}

	signed := &http.Request{
		Method: http.MethodGet, URL: presigned, Host: presigned.Host,
		Header: http.Header{"X-K8s-Aws-Id": {cluster}},
	}
	a.Expires = tokenLifetime
	if err = sigv4.Verify(signed, a, secret, emptyHash, false, time.Now()); err != nil {
		return "", errors.Join(ErrKubernetesToken, err)
	}

	return account, nil
}
