// Package sts implements the AWS Security Token Service without HTTP
// dependencies. Callers are identified by the account the gateway resolved for
// their access key: a configured key is that account's root user, an IAM
// access key is its user, a key that AssumeRole issued is its role session,
// and a key that GetSessionToken issued is the user that asked for it.
package sts

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sts"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	// sessionPrefix holds every issued session by access key. Access keys are
	// global, so sessions are not scoped to an account or region.
	sessionPrefix = "aws/sts/session/"

	minDuration     = 900
	defaultDuration = 3600
	// chainedDuration caps sessions that a role session assumes.
	chainedDuration = 3600
	maxDuration     = 43200
	// Session tokens last 12 hours by default and at most 36, or one hour for
	// the root user.
	userTokenDuration = 43200
	maxTokenDuration  = 129600
	rootTokenDuration = 3600
)

var (
	sessionName = regexp.MustCompile(`^[\w+=,.@-]{2,64}$`)
	externalID  = regexp.MustCompile(`^[\w+=,.@:/-]+$`)
)

var _ api.API = (*Service)(nil)

type Service struct {
	store storage.Store
	roles *iam.Service
	now   func() time.Time
	// accounts maps the configured access keys to their accounts, and
	// fallback is the account of the others; see UseAccounts.
	accounts map[string]string
	fallback string
}

// New returns STS. roles resolves the roles that AssumeRole assumes.
func New(store storage.Store, roles *iam.Service) *Service {
	return &Service{store: store, roles: roles, now: time.Now}
}

// session is a stored AssumeRole or GetSessionToken result: a role session,
// or else the user's.
type session struct {
	AccountID string
	Secret    string
	Token     string
	RoleARN   string `json:",omitempty"`
	RoleName  string `json:",omitempty"`
	RoleID    string `json:",omitempty"`
	Name      string `json:",omitempty"`
	UserARN   string `json:",omitempty"`
	UserID    string `json:",omitempty"`
	// CognitoIdentity is the Cognito identity ID whose token
	// AssumeRoleWithWebIdentity exchanged for the session.
	CognitoIdentity string `json:",omitempty"`
	Expires         int64  // Unix seconds
}

func (s *session) role() bool { return s.RoleARN != "" }

func (s *session) arn() string {
	if !s.role() {
		return s.UserARN
	}

	return "arn:aws:sts::" + s.AccountID + ":assumed-role/" + s.RoleName + "/" + s.Name
}

func (s *session) id() string {
	if !s.role() {
		return s.UserID
	}

	return s.RoleID + ":" + s.Name
}

func rootARN(account string) string { return "arn:aws:iam::" + account + ":root" }

func validation(format string, args ...any) error {
	return awserror.New("ValidationError", fmt.Sprintf(format, args...))
}

func random(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// callerSession returns the session m's access key belongs to, if it belongs
// to one in m's account.
func callerSession(tx storage.ReadTx, m *request.Metadata) (current *session, found bool, err error) {
	data, ok := tx.Get(sessionPrefix + m.AccessKey)
	if !ok || m.AccessKey == "" {
		return nil, false, nil
	}

	current = &session{}
	if err = json.Unmarshal(data, current); err != nil {
		return nil, false, fmt.Errorf("decode session: %w", err)
	}

	return current, current.AccountID == m.AccountID, nil
}

// Credentials returns the credentials of the session that AssumeRole issued
// for accessKey, or else of the IAM user's access key accessKey, for the
// gateway to resolve and verify.
func (s *Service) Credentials(ctx context.Context, accessKey string) (request.Credentials, bool, error) {
	var out request.Credentials
	found := false
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		data, ok := tx.Get(sessionPrefix + accessKey)
		if !ok {
			return nil
		}

		var stored session
		if err := json.Unmarshal(data, &stored); err != nil {
			return fmt.Errorf("decode session: %w", err)
		}

		found = true
		out = request.Credentials{
			Expires:         time.Unix(stored.Expires, 0),
			AccountID:       stored.AccountID,
			Secret:          stored.Secret,
			Token:           stored.Token,
			CognitoIdentity: stored.CognitoIdentity,
		}

		return nil
	})
	if err != nil || found {
		return out, found, err
	}

	key, found, err := s.roles.AccessKey(ctx, accessKey)
	return request.Credentials{AccountID: key.Account, Secret: key.Secret}, found, err
}

// callerUser returns the IAM user whose access key m carries, if the key is
// one of m's account.
func (s *Service) callerUser(ctx context.Context, m *request.Metadata) (iam.Key, bool, error) {
	if m.AccessKey == "" {
		return iam.Key{}, false, nil
	}

	key, found, err := s.roles.AccessKey(ctx, m.AccessKey)
	return key, found && key.Account == m.AccountID, err
}

// GetCallerIdentity returns the caller's role session, its IAM user, or the
// root user of the caller's account, which a configured access key acts as.
func (s *Service) GetCallerIdentity(
	ctx context.Context,
	_ *api.GetCallerIdentityRequest,
) (*api.GetCallerIdentityResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.GetCallerIdentityResponse{
		UserId:  new(m.AccountID),
		Account: new(m.AccountID),
		Arn:     new(rootARN(m.AccountID)),
	}
	key, isUser, err := s.callerUser(ctx, &m)
	if err != nil {
		return nil, err
	}

	if isUser {
		out.UserId, out.Arn = new(key.UserID), new(key.UserARN)
		return out, nil
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		current, found, lookupErr := callerSession(tx, &m)
		if lookupErr != nil || !found {
			return lookupErr
		}

		out.UserId = new(current.id())
		out.Arn = new(current.arn())
		return nil
	})

	return out, err
}

// AssumeRole issues temporary credentials for a role whose trust policy names
// the caller. Expired sessions are removed whenever a new one is issued.
func (s *Service) AssumeRole(ctx context.Context, in *api.AssumeRoleRequest) (*api.AssumeRoleResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkAssumeRole(in); guardErr != nil {
		return nil, guardErr
	}

	role, exists, err := s.roles.RoleByARN(ctx, *in.RoleArn)
	if err != nil {
		return nil, err
	}

	key, isUser, err := s.callerUser(ctx, &m)
	if err != nil {
		return nil, err
	}

	now := s.now()
	var out *api.AssumeRoleResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		current, inSession, lookupErr := callerSession(tx, &m)
		if lookupErr != nil {
			return lookupErr
		}

		chained := inSession && current.role()
		c := caller{
			account: m.AccountID, arns: []string{rootARN(m.AccountID)}, action: "sts:AssumeRole",
			context: map[string][]string{"sts:externalid": present(common.Value(in.ExternalId))},
		}
		switch {
		case chained:
			c.arns = append(c.arns, current.RoleARN, current.arn())
		case inSession && current.UserARN != rootARN(m.AccountID):
			c.arns = append(c.arns, current.UserARN)
		case isUser:
			c.arns = append(c.arns, key.UserARN)
		}

		if !exists || !trusts(role.TrustPolicy, &c) {
			return accessDenied(c.arns[len(c.arns)-1], *in.RoleArn)
		}

		duration, durationErr := sessionDuration(in.DurationSeconds, role.MaxSessionDuration, chained)
		if durationErr != nil {
			return durationErr
		}

		issued := &session{
			AccountID: role.Account, RoleARN: role.ARN, RoleName: role.Name, RoleID: role.ID,
			Name: *in.RoleSessionName, Expires: now.Add(duration).Unix(),
		}
		credentials, issueErr := issue(tx, issued, now)
		if issueErr != nil {
			return issueErr
		}

		out = &api.AssumeRoleResponse{
			Credentials: credentials,
			AssumedRoleUser: &api.AssumedRoleUser{
				AssumedRoleId: new(issued.id()),
				Arn:           new(issued.arn()),
			},
		}

		return nil
	})

	return out, err
}

// AssumeRoleForService issues credentials for a role to an AWS service
// principal, such as IoT's credentials provider, when the role's trust
// policy names that service. Other services call it in-process; no API
// operation reaches it.
func (s *Service) AssumeRoleForService(
	ctx context.Context,
	principal, roleARN, sessionName string,
	seconds int64,
) (accessKey, secret, token string, expires time.Time, err error) {
	role, exists, err := s.roles.RoleByARN(ctx, roleARN)
	if err != nil {
		return "", "", "", time.Time{}, err
	}

	c := &caller{service: principal, action: "sts:AssumeRole"}
	if !exists || !trusts(role.TrustPolicy, c) {
		return "", "", "", time.Time{}, denied(fmt.Sprintf("%s is not authorized to perform: sts:AssumeRole on "+
			"resource: %s", principal, roleARN))
	}

	duration, err := sessionDuration(&seconds, role.MaxSessionDuration, false)
	if err != nil {
		return "", "", "", time.Time{}, err
	}

	now := s.now()
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		issued := &session{
			AccountID: role.Account, RoleARN: role.ARN, RoleName: role.Name, RoleID: role.ID, Name: sessionName,
			Expires: now.Add(duration).Unix(),
		}
		credentials, issueErr := issue(tx, issued, now)
		if issueErr != nil {
			return issueErr
		}

		accessKey, secret, token = *credentials.AccessKeyId, *credentials.SecretAccessKey, *credentials.SessionToken
		expires = *credentials.Expiration
		return nil
	})

	return accessKey, secret, token, expires, err
}

// GetSessionToken issues temporary credentials that act as the caller: an
// IAM user or the account's root user.
func (s *Service) GetSessionToken(
	ctx context.Context,
	in *api.GetSessionTokenRequest,
) (*api.GetSessionTokenResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	key, isUser, err := s.callerUser(ctx, &m)
	if err != nil {
		return nil, err
	}

	issued := &session{AccountID: m.AccountID, UserARN: rootARN(m.AccountID), UserID: m.AccountID}
	seconds, limit := int64(rootTokenDuration), int64(rootTokenDuration)
	if isUser {
		issued.UserARN, issued.UserID = key.UserARN, key.UserID
		seconds, limit = userTokenDuration, maxTokenDuration
	}

	if in.DurationSeconds != nil {
		seconds = *in.DurationSeconds
	}

	switch {
	case seconds < minDuration || seconds > maxTokenDuration:
		return nil, validation("1 validation error detected: Value at 'durationSeconds' failed to satisfy "+
			"constraint: Member must have value between %d and %d", minDuration, maxTokenDuration)
	case seconds > limit:
		seconds = limit
	}

	now := s.now()
	var out *api.GetSessionTokenResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, inSession, lookupErr := callerSession(tx, &m)
		switch {
		case lookupErr != nil:
			return lookupErr
		case inSession:
			return denied("Cannot call GetSessionToken with session credentials")
		}

		issued.Expires = now.Add(time.Duration(seconds) * time.Second).Unix()
		credentials, issueErr := issue(tx, issued, now)
		if issueErr != nil {
			return issueErr
		}

		out = &api.GetSessionTokenResponse{Credentials: credentials}
		return nil
	})

	return out, err
}

// issue stores a new session under a new access key, removing expired ones,
// and returns its credentials.
func issue(tx storage.WriteTx, issued *session, now time.Time) (*api.Credentials, error) {
	if err := prune(tx, now); err != nil {
		return nil, err
	}

	issued.Secret = base64.StdEncoding.EncodeToString(random(30))
	issued.Token = base64.StdEncoding.EncodeToString(random(96))
	accessKey := "ASIA" + base32.StdEncoding.EncodeToString(random(10))
	// #nosec G117 -- the gateway verifies the session's signatures with its stored secret.
	data, err := json.Marshal(issued)
	if err != nil {
		return nil, err
	}

	tx.Put(sessionPrefix+accessKey, data)
	return &api.Credentials{
		AccessKeyId:     new(accessKey),
		SecretAccessKey: new(issued.Secret),
		SessionToken:    new(issued.Token),
		Expiration:      new(time.Unix(issued.Expires, 0).UTC()),
	}, nil
}

func checkAssumeRole(in *api.AssumeRoleRequest) error {
	if in.RoleArn == nil || len(*in.RoleArn) < 20 || len(*in.RoleArn) > 2048 {
		return validation("1 validation error detected: Value at 'roleArn' failed to satisfy constraint: " +
			"Member must have length between 20 and 2048")
	}

	if in.RoleSessionName == nil || !sessionName.MatchString(*in.RoleSessionName) {
		return validation("1 validation error detected: Value at 'roleSessionName' failed to satisfy constraint: " +
			"Member must satisfy regular expression pattern: [\\w+=,.@-]{2,64}")
	}

	if in.ExternalId != nil &&
		(len(*in.ExternalId) < 2 || len(*in.ExternalId) > 1224 || !externalID.MatchString(*in.ExternalId)) {
		return validation("1 validation error detected: Value at 'externalId' failed to satisfy constraint: " +
			"Member must satisfy regular expression pattern: [\\w+=,.@:\\/-]{2,1224}")
	}

	return nil
}

// sessionDuration returns the requested session length, within the role's
// maximum and the one-hour limit on role chaining.
func sessionDuration(requested *int64, roleMax int64, chained bool) (time.Duration, error) {
	seconds := int64(defaultDuration)
	if requested != nil {
		seconds = *requested
	}

	switch {
	case seconds < minDuration || seconds > maxDuration:
		return 0, validation("1 validation error detected: Value at 'durationSeconds' failed to satisfy "+
			"constraint: Member must have value between %d and %d", minDuration, maxDuration)
	case chained && seconds > chainedDuration:
		return 0, validation("The requested DurationSeconds exceeds the 1 hour session limit for roles " +
			"assumed by role chaining.")
	case seconds > roleMax:
		return 0, validation("The requested DurationSeconds exceeds the MaxSessionDuration set for this role.")
	}

	return time.Duration(seconds) * time.Second, nil
}

func denied(message string) error {
	return &awserror.Error{Code: "AccessDenied", Message: message, StatusCode: http.StatusForbidden}
}

func accessDenied(callerARN, roleARN string) error {
	return denied(fmt.Sprintf("User: %s is not authorized to perform: sts:AssumeRole on resource: %s",
		callerARN, roleARN))
}

// prune deletes the sessions that have expired by now.
func prune(tx storage.WriteTx, now time.Time) error {
	for _, k := range tx.Keys(sessionPrefix) {
		data, _ := tx.Get(k)
		var stored session
		if err := json.Unmarshal(data, &stored); err != nil {
			return fmt.Errorf("decode session: %w", err)
		}

		if stored.Expires <= now.Unix() {
			tx.Delete(k)
		}
	}

	return nil
}
