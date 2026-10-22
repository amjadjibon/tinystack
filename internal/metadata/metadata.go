package metadata

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/services/ec2"
	"github.com/amjadjibon/tinystack/internal/services/ecs"
	"github.com/amjadjibon/tinystack/internal/services/iam"
)

// AssumeRoleFunc issues temporary credentials for an AWS service principal.
type AssumeRoleFunc func(ctx context.Context, principal, roleARN, sessionName string, seconds int64) (
	accessKey, secret, token string, expires time.Time, err error,
)

// InstanceProfilesFunc looks up an IAM instance profile by name or ARN.
type InstanceProfilesFunc func(ctx context.Context, account, ref string) (iam.InstanceProfile, bool, error)

// Option configures the metadata service.
type Option func(*Service)

// WithAssumeRole sets the STS role assumption helper.
func WithAssumeRole(fn AssumeRoleFunc) Option {
	return func(s *Service) {
		s.assumeRole = fn
	}
}

// WithInstanceProfiles sets the IAM instance profile lookup helper.
func WithInstanceProfiles(fn InstanceProfilesFunc) Option {
	return func(s *Service) {
		s.instanceProfiles = fn
	}
}

// WithDefaultInstanceID sets the default EC2 instance ID for dedicated IMDS listeners.
func WithDefaultInstanceID(id string) Option {
	return func(s *Service) {
		s.defaultInstanceID = id
	}
}

// WithNow sets the time source.
func WithNow(now func() time.Time) Option {
	return func(s *Service) {
		s.now = now
	}
}

// Service serves EC2 IMDS and ECS Task Metadata v4 / Container Credentials endpoints.
type Service struct {
	instances         *ec2.Service
	containers        *ecs.Service
	assumeRole        AssumeRoleFunc
	instanceProfiles  InstanceProfilesFunc
	now               func() time.Time
	imdsTokens        map[string]time.Time
	defaultInstanceID string
	imdsMu            sync.RWMutex
	verifyIdentity    bool
}

// New returns an initialized metadata service.
func New(instances *ec2.Service, containers *ecs.Service, opts ...Option) *Service {
	s := &Service{
		instances:  instances,
		containers: containers,
		now:        time.Now,
		imdsTokens: make(map[string]time.Time),
	}

	for _, opt := range opts {
		opt(s)
	}

	return s
}

// WithVerifyIdentity refuses credentials on the shared gateway in verify mode.
func WithVerifyIdentity(verify bool) Option {
	return func(s *Service) { s.verifyIdentity = verify }
}

func (s *Service) generateToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return hex.EncodeToString(b), nil
}

// IMDSHandler returns an HTTP handler for EC2 IMDS (v1 and v2).
func (s *Service) IMDSHandler() http.Handler {
	return http.HandlerFunc(s.serveIMDS)
}

// ECSHandler returns an HTTP handler for ECS Task Metadata v4 and container credentials.
func (s *Service) ECSHandler() http.Handler {
	return http.HandlerFunc(s.serveECS)
}
