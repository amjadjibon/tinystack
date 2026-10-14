// Package codeartifact stores AWS CodeArtifact domains and repositories, with their upstreams, external connections
// and permissions policies, as metadata. No package is stored or served: GetRepositoryEndpoint returns a URL under
// TinyStack's address that nothing answers, and GetAuthorizationToken returns an opaque token that nothing checks.
package codeartifact

import (
	"fmt"
	"regexp"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codeartifact"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName    = "codeartifact"
	kindDomain     = "domain"
	kindRepository = "repository"

	statusActive = "Active"

	defaultPageSize = 50
	maxPageSize     = 1000
)

var _ api.API = (*Service)(nil)

var (
	// The naming rules of the CodeArtifact model.
	domainName     = regexp.MustCompile(`^[a-z][a-z0-9\-]{0,48}[a-z0-9]$`)
	repositoryName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._\-]{1,99}$`)
)

type Service struct {
	store storage.Store
}

func New(store storage.Store) *Service {
	return &Service{store: store}
}

func fault(code, format string, args ...any) error {
	return awserror.New(code, fmt.Sprintf(format, args...))
}

func invalid(format string, args ...any) error {
	return awserror.Validation(format, args...)
}

func domainKey(scope request.Scope, domain string) string {
	return scope.Key(serviceName, kindDomain, domain)
}

func repositoryKey(scope request.Scope, domain, repository string) string {
	return scope.Key(serviceName, kindRepository, domain+"/"+repository)
}

func domainARN(scope request.Scope, domain string) string {
	return fmt.Sprintf("arn:aws:codeartifact:%s:%s:domain/%s", scope.Region, scope.AccountID, domain)
}

func repositoryARN(scope request.Scope, domain, repository string) string {
	return fmt.Sprintf("arn:aws:codeartifact:%s:%s:repository/%s/%s", scope.Region, scope.AccountID, domain, repository)
}

func domainNotFound(domain string) error {
	return fault("ResourceNotFoundException", "Domain %s does not exist.", domain)
}

func repositoryNotFound(domain, repository string) error {
	return fault("ResourceNotFoundException", "Repository %s does not exist in domain %s.", repository, domain)
}

// checkDomain validates a domain name and the DomainOwner that goes with it. Only the calling account owns domains
// here, so another owner is rejected rather than ignored.
func checkDomain(scope request.Scope, domain, owner *string) (string, error) {
	if domain == nil || !domainName.MatchString(*domain) {
		return "", invalid("domain must be 2 to 50 lowercase letters, digits and hyphens, beginning with a letter.")
	}

	if owner != nil && *owner != scope.AccountID {
		return "", invalid("domainOwner must be the calling account, %s.", scope.AccountID)
	}

	return *domain, nil
}

func checkRepository(value *string) (string, error) {
	if value == nil || !repositoryName.MatchString(*value) {
		return "", invalid(
			"repository must be 2 to 100 letters, digits, periods, underscores and hyphens, beginning with a letter or digit.",
		)
	}

	return *value, nil
}

func checkDescription(value *string) error {
	if value != nil && len(*value) > 1000 {
		return invalid("description must be at most 1000 characters long.")
	}

	return nil
}

// pageSize reads maxResults, which defaults to 50.
func pageSize(in *int64) (int, error) {
	switch {
	case in == nil:
		return defaultPageSize, nil
	case *in < 1 || *in > maxPageSize:
		return 0, invalid("maxResults must be between 1 and %d.", maxPageSize)
	default:
		return int(*in), nil
	}
}

func invalidToken() error { return invalid("nextToken is not valid.") }
