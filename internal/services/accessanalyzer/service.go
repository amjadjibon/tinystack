package accessanalyzer

import (
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/accessanalyzer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName       = "accessanalyzer"
	arnNamespace      = "access-analyzer"
	kindAnalyzer      = "analyzer"
	kindArchiveRule   = "archiverule"
	statusActive      = "ACTIVE"
	defaultMaxResults = 100
)

// Service implements the IAM Access Analyzer control plane.
type Service struct {
	store storage.Store
}

// New returns an Access Analyzer service backed by store.
func New(store storage.Store) *Service {
	return &Service{store: store}
}

type analyzerRecord struct {
	CreatedAt     time.Time                  `json:"createdAt"`
	Tags          map[string]string          `json:"tags,omitempty"`
	Configuration *api.AnalyzerConfiguration `json:"configuration,omitempty"`
	ARN           string                     `json:"arn"`
	Name          string                     `json:"name"`
	Type          string                     `json:"type"`
	Status        string                     `json:"status"`
}

type archiveRuleRecord struct {
	CreatedAt    time.Time                `json:"createdAt"`
	UpdatedAt    time.Time                `json:"updatedAt"`
	Filter       map[string]api.Criterion `json:"filter"`
	AnalyzerName string                   `json:"analyzerName"`
	RuleName     string                   `json:"ruleName"`
}

func analyzerArn(scope request.Scope, name string) string {
	return fmt.Sprintf("arn:aws:%s:%s:%s:analyzer/%s", arnNamespace, scope.Region, scope.AccountID, name)
}

func analyzerKey(scope request.Scope, name string) string {
	return scope.Key(serviceName, kindAnalyzer, name)
}

func archiveRuleKey(scope request.Scope, analyzerName, ruleName string) string {
	return scope.Key(serviceName, kindArchiveRule, analyzerName+"/"+ruleName)
}

func archiveRulePrefix(scope request.Scope, analyzerName string) string {
	return scope.Prefix(serviceName, kindArchiveRule) + analyzerName + "/"
}

func notFound(kind, id string) error {
	return awserror.New("ResourceNotFoundException", fmt.Sprintf("%s %s not found.", kind, id))
}

func conflict(message string) error {
	return awserror.New("ConflictException", message)
}

func invalidToken() error {
	return awserror.Validation("NextToken is invalid.")
}

func pageSize(in *int64) int {
	if in == nil || *in <= 0 {
		return defaultMaxResults
	}

	return int(*in)
}

func parseAnalyzerArn(scope request.Scope, arnStr string) (string, error) {
	invalid := awserror.Validation("%q is not a valid Access Analyzer ARN.", arnStr)
	parts := strings.SplitN(arnStr, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != arnNamespace {
		return "", invalid
	}

	if parts[3] != scope.Region || parts[4] != scope.AccountID {
		return "", notFound("Analyzer", arnStr)
	}

	res := parts[5]
	kind, name, ok := strings.Cut(res, "/")
	if !ok || kind != kindAnalyzer || name == "" {
		return "", invalid
	}

	return name, nil
}

func validateAnalyzerConfiguration(config *api.AnalyzerConfiguration) error {
	if config == nil {
		return nil
	}

	if config.UnusedAccess != nil && config.UnusedAccess.UnusedAccessAge != nil {
		age := *config.UnusedAccess.UnusedAccessAge
		if age < 1 || age > 365 {
			return awserror.Validation("unusedAccessAge must be between 1 and 365 days.")
		}
	}

	return nil
}

func validateFilter(filter map[string]api.Criterion) error {
	if len(filter) == 0 {
		return awserror.Validation("Filter criteria cannot be empty.")
	}

	for key, criterion := range filter {
		hasOp := len(criterion.Eq) > 0 || len(criterion.Neq) > 0 || len(criterion.Contains) > 0 ||
			criterion.Exists != nil
		if !hasOp {
			return awserror.Validation("Criterion for %q must specify at least one condition.", key)
		}
	}

	return nil
}

var (
	_ api.API           = (*Service)(nil)
	_ catalog.Inventory = (*Service)(nil)
)
