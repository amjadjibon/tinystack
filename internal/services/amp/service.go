package amp

import (
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/amp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName                = "amp"
	arnNamespace               = "aps"
	kindWorkspace              = "workspace"
	kindRuleGroupsNamespace    = "rulegroupsnamespace"
	kindAlertManagerDefinition = "alertmanager"
	kindLoggingConfiguration   = "logging"
	statusActive               = "ACTIVE"
	statusCreating             = "CREATING"
	defaultMaxResults          = 100
)

// Service implements the Amazon Managed Service for Prometheus (AMP) control plane.
type Service struct {
	store storage.Store
}

// New returns an AMP service backed by store.
func New(store storage.Store) *Service {
	return &Service{store: store}
}

func key(scope request.Scope, kind, id string) string {
	return scope.Key(serviceName, kind, id)
}

func workspaceArn(scope request.Scope, workspaceID string) string {
	return fmt.Sprintf("arn:aws:%s:%s:%s:workspace/%s", arnNamespace, scope.Region, scope.AccountID, workspaceID)
}

func ruleGroupsNamespaceArn(scope request.Scope, workspaceID, name string) string {
	return fmt.Sprintf(
		"arn:aws:%s:%s:%s:rulegroupsnamespace/%s/%s",
		arnNamespace,
		scope.Region,
		scope.AccountID,
		workspaceID,
		name,
	)
}

func prometheusEndpoint(meta *request.Metadata, workspaceID string) string {
	base := meta.Endpoint
	if base == "" {
		base = fmt.Sprintf("https://aps-workspaces.%s.amazonaws.com", meta.Region)
	}

	return fmt.Sprintf("%s/workspaces/%s/", strings.TrimRight(base, "/"), workspaceID)
}

func notFound(kind, id string) error {
	return awserror.New("ResourceNotFoundException", fmt.Sprintf("%s %s was not found.", kind, id))
}

func conflict(message string) error {
	return awserror.New("ConflictException", message)
}

func invalidToken() error {
	return awserror.Validation("NextToken is not valid.")
}

func newWorkspaceID() string {
	return fmt.Sprintf(
		"ws-%s-%s-%s-%s-%s",
		common.RandomID(8),
		common.RandomID(4),
		common.RandomID(4),
		common.RandomID(4),
		common.RandomID(12),
	)
}

func checkWorkspaceExists(tx storage.ReadTx, scope request.Scope, workspaceID string) error {
	_, found, err := storage.Read[workspaceRecord](tx, key(scope, kindWorkspace, workspaceID))
	if err != nil {
		return err
	}

	if !found {
		return notFound("Workspace", workspaceID)
	}

	return nil
}

func pageSize(in *int64) (int, error) {
	if in == nil || *in == 0 {
		return defaultMaxResults, nil
	}

	if *in < 1 || *in > 1000 {
		return 0, awserror.Validation("MaxResults must be between 1 and 1000.")
	}

	return int(*in), nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

var (
	_ api.API           = (*Service)(nil)
	_ catalog.Inventory = (*Service)(nil)
)
