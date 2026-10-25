// Package bedrockagent implements local agent configuration and immutable version snapshots.
package bedrockagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagent"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/bedrock"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const serviceName = "bedrockagent"

// Service owns durable records. Lifecycle transitions advance after one Get observes the intermediate state.
type Service struct {
	store   storage.Store
	control *bedrock.Service
	caller  *catalog.Caller
}

// New constructs the agent control plane.
func New(store storage.Store, control *bedrock.Service) *Service {
	return &Service{store: store, control: control}
}

// UseCaller enables reference validation against registered services.
func (s *Service) UseCaller(c *catalog.Caller) { s.caller = c }

type record struct {
	Agent         *api.Agent              `json:"agent,omitempty"`
	Alias         *api.AgentAlias         `json:"alias,omitempty"`
	Version       *api.AgentVersion       `json:"version,omitempty"`
	Group         *api.AgentActionGroup   `json:"group,omitempty"`
	KnowledgeBase *api.KnowledgeBase      `json:"knowledgeBase,omitempty"`
	DataSource    *api.DataSource         `json:"dataSource,omitempty"`
	Association   *api.AgentKnowledgeBase `json:"association,omitempty"`
	Tags          map[string]string       `json:"tags,omitempty"`
	Token         string                  `json:"token,omitempty"`
	NextVersion   int                     `json:"nextVersion,omitempty"`
}

func invalid(format string, args ...any) error {
	return awserror.New("ValidationException", fmt.Sprintf(format, args...))
}

func missing() error { return awserror.New("ResourceNotFoundException", "Resource does not exist.") }

func conflict(message string) error { return awserror.New("ConflictException", message) }

func arn(scope request.Scope, kind, id string) string {
	return fmt.Sprintf("arn:aws:bedrock:%s:%s:%s/%s", scope.Region, scope.AccountID, kind, id)
}

func read(tx storage.ReadTx, scope request.Scope, kind, id string) (*record, error) {
	r, ok, err := storage.Read[record](tx, scope.Key(serviceName, kind, id))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing()
	}

	return r, nil
}

func (s *Service) mutate(
	ctx context.Context,
	kind, id string,
	fn func(storage.WriteTx, request.Scope, *record) error,
) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, kind, id)
		if e != nil {
			return e
		}

		if mutateErr := fn(tx, scope, r); mutateErr != nil {
			return mutateErr
		}

		out = common.Clone(r)
		if kind == kindAgent && common.Value(r.Agent.AgentStatus) != statusDeleting {
			if e := storage.Write(
				tx,
				scope.Key(serviceName, kindVersion, id+"/DRAFT"),
				&record{Version: agentVersion(r.Agent)},
			); e != nil {
				return e
			}
		}

		return storage.Write(tx, scope.Key(serviceName, kind, id), r)
	})
	return out, err
}

func (s *Service) lookup(ctx context.Context, kind, id string) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error { var e error; out, e = read(tx, scope, kind, id); return e })
	return out, err
}

func limit(n *int64) int {
	if n == nil {
		return 100
	}

	return int(*n)
}

func (s *Service) rows(ctx context.Context, kind, prefix string) ([]*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var rows []*record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		rows, e = storage.Records[record](tx, scope.Prefix(serviceName, kind)+prefix)
		return e
	})
	return rows, err
}

func draft(version *string) error {
	if common.Value(version) != versionDraft {
		return invalid("Only DRAFT can be modified.")
	}

	return nil
}

func (s *Service) reference(ctx context.Context, service, operation string, params map[string]any) error {
	if s.caller == nil {
		return invalid("Reference validation is unavailable.")
	}

	_, err := s.caller.Call(ctx, service, operation, params)
	if err != nil {
		return invalid("Invalid %s reference: %v", service, err)
	}

	return nil
}

func roleName(value string) string {
	_, name, _ := strings.Cut(value, ":role/")
	return name[strings.LastIndex(name, "/")+1:]
}

var _ api.API = (*Service)(nil)
