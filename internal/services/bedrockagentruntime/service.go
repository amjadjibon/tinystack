// Package bedrockagentruntime executes user-authored agent fixtures and stores sessions.
package bedrockagentruntime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagentruntime"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/bedrockagent"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const serviceName = "bedrockagentruntime"

// Service executes only fixture-defined steps; it never synthesizes model output.
type Service struct {
	store  storage.Store
	agents *bedrockagent.Service
	caller *catalog.Caller
}

// New constructs the runtime.
func New(store storage.Store, agents *bedrockagent.Service) *Service {
	return &Service{store: store, agents: agents}
}

// UseCaller enables local Lambda execution and KMS validation.
func (s *Service) UseCaller(c *catalog.Caller) { s.caller = c }

func invalid(format string, args ...any) error {
	return awserror.New("ValidationException", fmt.Sprintf(format, args...))
}

func missing() error { return awserror.New("ResourceNotFoundException", "Resource does not exist.") }

func conflict(message string) error { return awserror.New("ConflictException", message) }

func uuid() string {
	return common.RandomID(
		8,
		"0123456789abcdef",
	) + "-" + common.RandomID(
		4,
		"0123456789abcdef",
	) + "-" + common.RandomID(
		4,
		"0123456789abcdef",
	) + "-" + common.RandomID(
		4,
		"0123456789abcdef",
	) + "-" + common.RandomID(
		12,
		"0123456789abcdef",
	)
}

func limit(n *int64) int {
	if n == nil {
		return 100
	}

	return int(*n)
}

func sessionARN(scope request.Scope, id string) string {
	return fmt.Sprintf("arn:aws:bedrock:%s:%s:session/%s", scope.Region, scope.AccountID, id)
}

func sessionID(scope request.Scope, value string) (string, error) {
	if strings.HasPrefix(value, "arn:") {
		id, ok := strings.CutPrefix(value, sessionARN(scope, ""))
		if !ok {
			return "", missing()
		}

		return id, nil
	}

	return value, nil
}

func readSession(tx storage.ReadTx, scope request.Scope, id string) (*api.GetSessionResponse, error) {
	r, ok, err := storage.Read[api.GetSessionResponse](tx, scope.Key(serviceName, kindSession, id))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing()
	}

	// AWS session data is retained for 30 days; idle sessions expire after one hour.
	if r.CreatedAt != nil && time.Since(*r.CreatedAt) >= 30*24*time.Hour {
		return nil, missing()
	}

	if common.Value(r.SessionStatus) == statusActive && r.LastUpdatedAt != nil &&
		time.Since(*r.LastUpdatedAt) >= time.Hour {
		r.SessionStatus = new("EXPIRED")
	}

	return r, nil
}

func (s *Service) mutateSession(
	ctx context.Context,
	identifier *string,
	active bool,
	fn func(storage.WriteTx, request.Scope, *api.GetSessionResponse) error,
) (*api.GetSessionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id, err := sessionID(scope, common.Value(identifier))
	if err != nil {
		return nil, err
	}

	var out *api.GetSessionResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readSession(tx, scope, id)
		if e != nil {
			return e
		}

		if active && common.Value(r.SessionStatus) != statusActive {
			return conflict("Session is not ACTIVE.")
		}

		if mutateErr := fn(tx, scope, r); mutateErr != nil {
			return mutateErr
		}

		out = common.Clone(r)
		return storage.Write(tx, scope.Key(serviceName, kindSession, id), r)
	})
	return out, err
}

var _ api.API = (*Service)(nil)
