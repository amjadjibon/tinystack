// Package verifiedpermissions implements the Amazon Verified Permissions control plane in TinyStack.
// It supports policy store CRUD, schema management, static policy CRUD, and resource tagging.
// Authorization APIs and template-linked policies are out of scope.
// When ValidationSettings mode is STRICT, the mode is stored and returned, but Cedar schema validation
// of policy statements is not performed in this records-only implementation.
package verifiedpermissions

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/arn"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/verifiedpermissions"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName             = "verifiedpermissions"
	kindPolicyStore         = "policystore"
	kindSchema              = "schema"
	kindPolicy              = "policy"
	kindToken               = "token"
	kindPolicyToken         = "policytoken"
	resourceTypePolicyStore = "POLICY_STORE"
	resourceTypePolicy      = "POLICY"
	resourceTypeSchema      = "SCHEMA"
	policyTypeStatic        = "STATIC"
	defaultPageLimit        = 10
	maxPageLimit            = 50
	maxDescriptionLength    = 150
)

var _ api.API = (*Service)(nil)

type Service struct {
	store storage.Store
}

func New(store storage.Store) *Service {
	return &Service{store: store}
}

type policyStoreRecord struct {
	CreatedDate        time.Time
	LastUpdatedDate    time.Time
	ValidationSettings *api.ValidationSettings
	Description        *string
	Tags               map[string]string
	PolicyStoreId      string
	Arn                string
}

type schemaRecord struct {
	CreatedDate     time.Time
	LastUpdatedDate time.Time
	PolicyStoreId   string
	CedarJson       string
	Namespaces      []string
}

type policyRecord struct {
	CreatedDate     time.Time
	LastUpdatedDate time.Time
	Description     *string
	PolicyId        string
	PolicyStoreId   string
	PolicyType      string
	Statement       string
	Effect          string
}

type tokenRecord struct {
	PolicyStoreId string
	Description   *string
	Mode          string
}

type policyTokenRecord struct {
	Description *string
	PolicyId    string
	Statement   string
}

func notFound(resourceType, resourceID, message string) error {
	err := awserror.New("ResourceNotFoundException", message)
	err.Fields = map[string]any{
		"resourceId":   resourceID,
		"resourceType": resourceType,
	}

	return err
}

func policyStoreARN(scope request.Scope, id string) string {
	return fmt.Sprintf("arn:aws:verifiedpermissions:%s:%s:policy-store/%s", scope.Region, scope.AccountID, id)
}

func policyARN(scope request.Scope, storeID, policyID string) string {
	return fmt.Sprintf(
		"arn:aws:verifiedpermissions:%s:%s:policy-store/%s/policy/%s",
		scope.Region,
		scope.AccountID,
		storeID,
		policyID,
	)
}

func parsePolicyStoreARN(scope request.Scope, raw string) (string, error) {
	parsed, err := arn.Parse(raw)
	if err != nil || parsed.Partition != "aws" || parsed.Service != serviceName ||
		parsed.Region != scope.Region || parsed.AccountID != scope.AccountID {
		return "", notFound(resourceTypePolicyStore, raw, fmt.Sprintf("could not find policy store with ARN %s", raw))
	}

	resType, id, hasSlash := strings.Cut(parsed.Resource, "/")
	if !hasSlash || resType != "policy-store" || id == "" {
		return "", notFound(resourceTypePolicyStore, raw, fmt.Sprintf("could not find policy store with ARN %s", raw))
	}

	return id, nil
}

// effectPattern finds a policy's effect keyword after any leading comments and annotations such as @id("...").
var effectPattern = regexp.MustCompile(`^(?:\s+|//[^\n]*\n|@\w+\s*\([^)]*\))*(permit|forbid)\s*\(`)

func parseEffect(statement string) (string, error) {
	m := effectPattern.FindStringSubmatch(statement)
	if m == nil {
		return "", awserror.Validation("Policy statement must be a permit or forbid policy")
	}

	if m[1] == "permit" {
		return "Permit", nil
	}

	return "Forbid", nil
}

// pageLimit applies the documented default of 10 and maximum of 50 results per page.
func pageLimit(maxResults *int64) (int, error) {
	if maxResults == nil {
		return defaultPageLimit, nil
	}

	if *maxResults < 1 || *maxResults > maxPageLimit {
		return 0, awserror.Validation("maxResults must be between 1 and %d", maxPageLimit)
	}

	return int(*maxResults), nil
}
