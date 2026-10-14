// Package codeconnections stores AWS CodeConnections connections and hosts as metadata. A connection stays PENDING,
// as it does in AWS until a person completes the provider handshake in the console. No provider is ever contacted,
// so nothing moves a connection to AVAILABLE.
package codeconnections

import (
	"fmt"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codeconnections"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName    = "codeconnections"
	kindConnection = "connection"
	kindHost       = "host"

	statusPending = "PENDING"

	// The only provider a host can stand for, from the CreateHost reference.
	hostProvider = "GitHubEnterpriseServer"

	maxResults = 100
)

// providers are the ProviderType values of the model.
var providers = []string{"Bitbucket", "GitHub", "GitHubEnterpriseServer", "GitLab", "GitLabSelfManaged", "AzureDevOps"}

var _ api.API = (*Service)(nil)

type Service struct {
	store storage.Store
}

func New(store storage.Store) *Service {
	return &Service{store: store}
}

func fault(code, format string, args ...any) error {
	return awserror.New(code, fmt.Sprintf(format, args...))
}

func key(scope request.Scope, kind, id string) string {
	return scope.Key(serviceName, kind, id)
}

func arn(scope request.Scope, kind, id string) string {
	return fmt.Sprintf("arn:aws:%s:%s:%s:%s/%s", serviceName, scope.Region, scope.AccountID, kind, id)
}

func notFound(kind, ref string) error {
	return fault("ResourceNotFoundException", "The %s %s was not found.", kind, ref)
}

// optional returns nil for an empty value, so unset members stay out of responses.
func optional(v string) *string {
	if v == "" {
		return nil
	}

	return &v
}

// pageSize reads MaxResults, which defaults to the maximum.
func pageSize(in *int64) (int, error) {
	switch {
	case in == nil || *in == 0:
		return maxResults, nil
	case *in < 0 || *in > maxResults:
		return 0, awserror.Validation("MaxResults must be between 1 and %d.", maxResults)
	default:
		return int(*in), nil
	}
}

func invalidToken() error { return awserror.Validation("NextToken is not valid.") }

// checkLength applies a model length rule to a member.
func checkLength(what string, value *string, maxLen int) (string, error) {
	if value == nil || *value == "" || len(*value) > maxLen {
		return "", awserror.Validation("%s must be 1 to %d characters long.", what, maxLen)
	}

	return *value, nil
}

func checkProvider(value string) error {
	if !slices.Contains(providers, value) {
		return awserror.Validation("ProviderType %q is not supported.", value)
	}

	return nil
}

// split reads the kind and ID out of the ARN of a connection or host in the request's account and Region.
func split(scope request.Scope, value *string) (kind, id string, err error) {
	invalid := awserror.Validation("%q is not a valid CodeConnections ARN.", common.Value(value))
	if value == nil {
		return "", "", invalid
	}

	parts := strings.SplitN(*value, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != serviceName {
		return "", "", invalid
	}

	kind, id, ok := strings.Cut(parts[5], "/")
	if !ok || id == "" || kind != kindConnection && kind != kindHost {
		return "", "", invalid
	}

	if parts[3] != scope.Region || parts[4] != scope.AccountID {
		return "", "", notFound(kind, *value)
	}

	return kind, id, nil
}

// expect reads the ID of a resource of one kind from its ARN.
func expect(scope request.Scope, kind string, value *string) (string, error) {
	got, id, err := split(scope, value)
	if err != nil {
		return "", err
	}

	if got != kind {
		return "", awserror.Validation("%q is not the ARN of a %s.", *value, kind)
	}

	return id, nil
}
