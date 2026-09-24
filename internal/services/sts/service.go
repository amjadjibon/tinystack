// Package sts implements the AWS Security Token Service without HTTP
// dependencies. Callers are identified by the account the gateway resolved for
// their access key: a long-term key is that account's root user, and a key
// that AssumeRole issued is its role session.
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
}

// New returns STS. roles resolves the roles that AssumeRole assumes.
func New(store storage.Store, roles *iam.Service) *Service {
	return &Service{store: store, roles: roles, now: time.Now}
}

// session is a stored AssumeRole result.
type session struct {
	AccountID string
	Secret    string
	Token     string
	RoleARN   string
	RoleName  string
	RoleID    string
	Name      string
	Expires   int64 // Unix seconds
}

func (s *session) arn() string {
	return "arn:aws:sts::" + s.AccountID + ":assumed-role/" + s.RoleName + "/" + s.Name
}

func identity(ctx context.Context) (request.Metadata, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" {
		return m, fmt.Errorf("missing request identity")
	}

	return m, nil
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

// Session returns the session that AssumeRole issued for accessKey, for the
// gateway to resolve and verify temporary credentials.
func (s *Service) Session(ctx context.Context, accessKey string) (request.Session, bool, error) {
	var out request.Session
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
		out = request.Session{
			Expires:   time.Unix(stored.Expires, 0),
			AccountID: stored.AccountID,
			Secret:    stored.Secret,
			Token:     stored.Token,
		}

		return nil
	})

	return out, found, err
}

// GetCallerIdentity returns the caller's role session, or the root user of the
// caller's account: TinyStack maps a long-term access key to an account, not
// to a user within it.
func (s *Service) GetCallerIdentity(
	ctx context.Context,
	_ *api.GetCallerIdentityRequest,
) (*api.GetCallerIdentityResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.GetCallerIdentityResponse{
		UserId:  new(m.AccountID),
		Account: new(m.AccountID),
		Arn:     new(rootARN(m.AccountID)),
	}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		current, found, lookupErr := callerSession(tx, &m)
		if lookupErr != nil || !found {
			return lookupErr
		}

		out.UserId = new(current.RoleID + ":" + current.Name)
		out.Arn = new(current.arn())
		return nil
	})

	return out, err
}

// AssumeRole issues temporary credentials for a role whose trust policy names
// the caller. Expired sessions are removed whenever a new one is issued.
func (s *Service) AssumeRole(ctx context.Context, in *api.AssumeRoleRequest) (*api.AssumeRoleResponse, error) {
	m, err := identity(ctx)
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

	now := s.now()
	var out *api.AssumeRoleResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		current, chained, lookupErr := callerSession(tx, &m)
		if lookupErr != nil {
			return lookupErr
		}

		c := caller{account: m.AccountID, arns: []string{rootARN(m.AccountID)}}
		if chained {
			c.arns = append(c.arns, current.RoleARN, current.arn())
		}

		if !exists || !trusts(role.TrustPolicy, c, value(in.ExternalId)) {
			return accessDenied(c.arns[len(c.arns)-1], *in.RoleArn)
		}

		duration, durationErr := sessionDuration(in.DurationSeconds, role.MaxSessionDuration, chained)
		if durationErr != nil {
			return durationErr
		}

		if pruneErr := prune(tx, now); pruneErr != nil {
			return pruneErr
		}

		issued := &session{
			AccountID: role.Account,
			Secret:    base64.StdEncoding.EncodeToString(random(30)),
			Token:     base64.StdEncoding.EncodeToString(random(96)),
			RoleARN:   role.ARN,
			RoleName:  role.Name,
			RoleID:    role.ID,
			Name:      *in.RoleSessionName,
			Expires:   now.Add(duration).Unix(),
		}
		accessKey := "ASIA" + base32.StdEncoding.EncodeToString(random(10))
		// #nosec G117 -- the gateway verifies the session's signatures with its stored secret.
		data, marshalErr := json.Marshal(issued)
		if marshalErr != nil {
			return marshalErr
		}

		tx.Put(sessionPrefix+accessKey, data)
		out = &api.AssumeRoleResponse{
			Credentials: &api.Credentials{
				AccessKeyId:     new(accessKey),
				SecretAccessKey: new(issued.Secret),
				SessionToken:    new(issued.Token),
				Expiration:      new(time.Unix(issued.Expires, 0).UTC()),
			},
			AssumedRoleUser: &api.AssumedRoleUser{
				AssumedRoleId: new(issued.RoleID + ":" + issued.Name),
				Arn:           new(issued.arn()),
			},
		}

		return nil
	})

	return out, err
}

func value(p *string) string {
	if p == nil {
		return ""
	}

	return *p
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

func accessDenied(callerARN, roleARN string) error {
	return &awserror.Error{
		Code: "AccessDenied",
		Message: fmt.Sprintf("User: %s is not authorized to perform: sts:AssumeRole on resource: %s",
			callerARN, roleARN),
		StatusCode: http.StatusForbidden,
	}
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
