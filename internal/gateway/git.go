package gateway

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/aws/sigv4"
	"github.com/amjadjibon/tinystack/internal/config"
)

// gitSkew is how far a credential helper's timestamp may differ from the clock, as for SigV4.
const gitSkew = 15 * time.Minute

// codecommitService is CodeCommit's signing name and IAM prefix.
const codecommitService = "codecommit"

// gitHost matches the hosts CodeCommit serves git over HTTPS on, with the Region in the first group.
var gitHost = regexp.MustCompile(
	`^(?:vpce-[^.]+\.)?git-codecommit(?:-fips)?\.([a-z0-9-]+)\.(?:vpce\.)?amazonaws\.com(?:\.cn)?$`)

// Git serves CodeCommit's git smart HTTP transport for one repository, in the scope the request names. rest is
// what follows the repository name, such as "info/refs".
type Git interface {
	ServeGit(w http.ResponseWriter, r *http.Request, scope request.Scope, repository, rest string)
}

// UseGit serves git clone, fetch and push under /v1/repos/<repository> to requests that are not SigV4-signed,
// and everything on a git-codecommit.<region>.amazonaws.com host. Anonymous S3 requests to a bucket named "v1"
// are then CodeCommit's. Call it before serving requests.
func (s *Server) UseGit(git Git) {
	s.git = git
}

// gitPath splits /v1/repos/<repository>/<rest>.
func gitPath(path string) (repository, rest string, ok bool) {
	tail, ok := strings.CutPrefix(path, "/v1/repos/")
	if !ok {
		return "", "", false
	}

	repository, rest, _ = strings.Cut(tail, "/")
	return repository, rest, repository != ""
}

// gitRegion returns the Region of a CodeCommit git host, and whether the host is one.
func gitRegion(host string) (string, bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	match := gitHost.FindStringSubmatch(strings.ToLower(strings.TrimSuffix(host, ".")))
	if match == nil {
		return "", false
	}

	return match[1], true
}

// serveGit serves a git request, reporting whether the request was one.
//
// The Region is the git host's, else defaults.region. The account is that of the Basic user name, which git
// credential helpers fill with an access key (a session's token follows a "%"): configured and issued keys,
// then a 12-digit key, then defaults.account. identity.mode: verify checks the password the AWS CLI's
// credential helper derives from the key's secret.
func (s *Server) serveGit(w http.ResponseWriter, r *http.Request) bool {
	if s.git == nil {
		return false
	}

	region, onHost := gitRegion(r.Host)
	repository, rest, onPath := gitPath(r.URL.Path)
	switch {
	case onHost && !onPath:
		http.NotFound(w, r)
		return true
	case !onPath:
		return false
	case !onHost:
		// Signed requests to a bucket named "v1" are S3's, and a custom domain's paths are its API's.
		if strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-") || sigv4.Presigned(r.URL.Query()) {
			return false
		}

		if _, isDomain := s.domainHost(r); isDomain {
			return false
		}

		region = s.cfg.Defaults.Region
	}

	user, password, hasAuth := r.BasicAuth()
	key, token, _ := strings.Cut(user, "%")
	if s.cfg.Identity.Mode == config.IdentityVerify {
		if !hasAuth {
			w.Header().Set("WWW-Authenticate", `Basic realm="CodeCommit"`)
			http.Error(w, "Authentication required: use the AWS CLI credential helper.", http.StatusUnauthorized)
			return true
		}

		if !s.verifyGit(r, key, token, password, region, repository) {
			http.Error(w, "Access denied.", http.StatusForbidden)
			return true
		}
	}

	scope := request.Scope{AccountID: s.gitAccount(r.Context(), key), Region: region}
	mode := s.cfg.Identity.Authorization
	action := gitAction(r, rest)
	if action == "" || mode == "" || mode == config.AuthorizationOff {
		s.git.ServeGit(w, r, scope, repository, rest)
		return true
	}

	start := time.Now()
	body := &capturingBody{ReadCloser: r.Body}
	r.Body = body
	cw := &capturingWriter{ResponseWriter: w}
	record := s.authorizeGit(r, scope, repository, action, key, token)
	if record.Allowed || mode == config.AuthorizationSoft {
		s.git.ServeGit(cw, r, scope, repository, rest)
	} else {
		http.Error(cw, record.Message, http.StatusForbidden)
	}

	s.requests.Add(Record{
		Authorization: record, Time: start.UTC(), RequestID: requestID(), Method: r.Method, Path: r.URL.Path,
		Service: codecommitService, Operation: action, Protocol: "git", Account: scope.AccountID, Region: scope.Region,
		Status: cmp.Or(cw.status, http.StatusOK), RequestSize: body.size, ResponseSize: cw.size,
		DurationMS: float64(time.Since(start).Microseconds()) / 1000,
	})

	return true
}

// requestID returns a random request ID, as classify does.
func requestID() string {
	var random [16]byte
	_, _ = rand.Read(random[:])
	return hex.EncodeToString(random[:])
}

// gitAction returns the IAM action a git request needs: codecommit:GitPull to clone and fetch, GitPush to push,
// or "" for a path that is neither, which the service answers itself.
func gitAction(r *http.Request, rest string) string {
	service := rest
	if rest == "info/refs" && r.Method == http.MethodGet {
		service = r.URL.Query().Get("service")
	} else if r.Method != http.MethodPost {
		return ""
	}

	switch service {
	case "git-upload-pack":
		return "GitPull"
	case "git-receive-pack":
		return "GitPush"
	default:
		return ""
	}
}

// gitPrincipal resolves who a git client is from the access key and session token of its Basic user name, as
// the gateway resolves a SigV4 caller: a configured key acts as its account's root, a key TinyStack issued acts
// as its IAM user or role, and a key it does not know, or whose session token or expiry does not hold, is
// anonymous.
func (s *Server) gitPrincipal(ctx context.Context, key, token, account string) request.Principal {
	_, issued, ok := s.gitKey(ctx, key, token)
	switch {
	case ok && issued != nil:
		return issued.Principal
	case ok:
		arn := "arn:aws:iam::" + account + ":root"
		return request.Principal{
			Kind: request.PrincipalRoot, Account: account, UserID: account, ARN: arn, PolicyARN: arn,
		}
	default:
		return request.Principal{Kind: request.PrincipalAnonymous, ARN: "anonymous"}
	}
}

// authorizeGit decides a git request with the gateway's policy evaluation: the action on the repository's ARN,
// against the caller's identity policies, boundary and session policy. The decision is logged like any other.
func (s *Server) authorizeGit(
	r *http.Request, scope request.Scope, repository, action, key, token string,
) *AuthorizationRecord {
	mode := s.cfg.Identity.Authorization
	principal := s.gitPrincipal(r.Context(), key, token, scope.AccountID)
	snapshot, lookupFailed := s.identitySnapshot(r.Context(), &principal)
	principal = snapshot.Principal
	values := authorizationValues(r, &principal, snapshot.Tags, scope.Region)
	ctx := request.WithMetadata(r.Context(), request.Metadata{
		Scope: scope, Service: codecommitService, Operation: action, Protocol: "git",
		Principal: &principal, AuthorizationValues: values,
	})
	arn := "arn:aws:codecommit:" + scope.Region + ":" + scope.AccountID + ":" + repository
	record := s.evaluate(ctx, mode, &evaluation{
		service:      &catalog.Service{IAMPrefix: codecommitService},
		op:           &catalog.Operation{Name: action, IAMAction: codecommitService + ":" + action},
		snapshot:     snapshot,
		values:       values,
		resources:    []catalog.AuthorizationResource{{ARN: arn}},
		serviceName:  codecommitService,
		lookupFailed: lookupFailed,
	})

	s.logger.Info("authorization", "mode", mode, "principal", record.Principal, "allowed", record.Allowed,
		"decision", record.Message, "checks", record.Checks)
	return record
}

// gitAccount returns the account an access key acts in; see accountFromKey for the order.
func (s *Server) gitAccount(ctx context.Context, key string) string {
	account, mapped, ok := s.keyAccount(key)
	if !mapped && key != "" && s.credentials != nil {
		if c, found, err := s.credentials(ctx, key); err == nil && found {
			return c.AccountID
		}
	}

	if ok {
		return account
	}

	return s.cfg.Defaults.Account
}

// gitKey resolves an access key a git client presented: the secret of a configured key, or the credentials TinyStack
// issued for it. Issued credentials must carry the session token the client sent and must not have expired. ok is
// false for a key TinyStack does not know.
func (s *Server) gitKey(ctx context.Context, key, token string) (secret string, issued *request.Credentials, ok bool) {
	if secret, ok = s.cfg.Identity.SecretKeys[key]; ok {
		return secret, nil, true
	}

	if _, mapped := s.cfg.Identity.Accounts[key]; mapped {
		return "", nil, true
	}

	if key == "" || s.credentials == nil {
		return "", nil, false
	}

	credentials, found, err := s.credentials(ctx, key)
	if err != nil || !found || subtle.ConstantTimeCompare([]byte(token), []byte(credentials.Token)) != 1 ||
		!credentials.Expires.IsZero() && !time.Now().Before(credentials.Expires) {
		return "", nil, false
	}

	return credentials.Secret, &credentials, true
}

// verifyGit checks the password the AWS CLI's `aws codecommit credential-helper` derives: the UTC time as
// YYYYMMDDTHHMMSS, "Z", and a SigV4 signature, with the codecommit service, of a canonical request for the method
// GIT on the repository's path and the host without its port.
func (s *Server) verifyGit(r *http.Request, key, token, password, region, repository string) bool {
	secret, _, ok := s.gitKey(r.Context(), key, token)
	if !ok || secret == "" {
		return false
	}

	stamp, signature, found := strings.Cut(password, "Z")
	issued, err := time.Parse("20060102T150405", stamp)
	if !found || err != nil {
		return false
	}

	if skew := time.Since(issued); skew > gitSkew || skew < -gitSkew {
		return false
	}

	host := r.Host
	if h, _, splitErr := net.SplitHostPort(host); splitErr == nil {
		host = h
	}

	canonical := "GIT\n/v1/repos/" + repository + "\n\nhost:" + host + "\n\nhost\n"
	digest := sha256.Sum256([]byte(canonical))
	date := stamp[:8]
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + date + "/" + region + "/codecommit/aws4_request\n" +
		hex.EncodeToString(digest[:])
	want := hex.EncodeToString(sigv4.HMACSHA256(sigv4.SigningKey(secret, date, region, "codecommit"), toSign))

	return subtle.ConstantTimeCompare([]byte(signature), []byte(want)) == 1
}
