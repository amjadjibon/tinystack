// Package opensearch stores Amazon OpenSearch Service domains and tags.
// It provides records-only control-plane operations and does not run
// an OpenSearch or Elasticsearch search engine runtime.
package opensearch

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/opensearch"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName         = "opensearch"
	domainKind          = "domain"
	maxDomains          = 100
	defaultEngineVer    = "OpenSearch_2.11"
	defaultInstanceType = "m5.large.search"
	defaultTLSPolicy    = "Policy-Min-TLS-1-0-2019-07"
)

var (
	_ api.API = (*Service)(nil)

	domainNameRegexp    = regexp.MustCompile(`^[a-z][a-z0-9-]+$`)
	engineVersionRegexp = regexp.MustCompile(`^(OpenSearch|Elasticsearch)_\d+\.\d+$`)
)

type (
	Service struct{ store storage.Store }

	domainRecord struct {
		Status        *api.DomainStatus
		Tags          map[string]string
		CreatedAt     time.Time
		UpdatedAt     time.Time
		UpdateVersion int64
	}
)

func New(store storage.Store) *Service { return &Service{store: store} }

func invalid(format string, args ...any) error {
	return awserror.New("ValidationException", fmt.Sprintf(format, args...))
}

func notFound(format string, args ...any) error {
	err := awserror.New("ResourceNotFoundException", fmt.Sprintf(format, args...))
	err.StatusCode = 409
	return err
}

func alreadyExists(format string, args ...any) error {
	err := awserror.New("ResourceAlreadyExistsException", fmt.Sprintf(format, args...))
	err.StatusCode = 409
	return err
}

func limitExceeded(format string, args ...any) error {
	err := awserror.New("LimitExceededException", fmt.Sprintf(format, args...))
	err.StatusCode = 409
	return err
}

func checkTagLimit(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return limitExceeded("%s", err.Error())
	}

	return invalid("%s", err.Error())
}

func tagMap(tags []api.Tag) (map[string]string, error) {
	return tagging.Map(tags, func(t api.Tag) (*string, *string) {
		return t.Key, t.Value
	}, invalid("Tag key and value are required"), func(key string) error {
		return invalid("Duplicate tag key %s", key)
	})
}

func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(k, v string) api.Tag {
		return api.Tag{Key: new(k), Value: new(v)}
	})
}

func validateDomainName(name string) error {
	if len(name) < 3 || len(name) > 28 || !domainNameRegexp.MatchString(name) {
		return invalid("Domain name must be 3-28 characters long and match ^[a-z][a-z0-9-]+$")
	}

	return nil
}

func validateEngineVersion(version string) error {
	if !engineVersionRegexp.MatchString(version) {
		return invalid("EngineVersion %q must match ^(OpenSearch|Elasticsearch)_[0-9]+.[0-9]+$", version)
	}

	return nil
}

func domainARN(region, account, name string) string {
	return fmt.Sprintf("arn:aws:es:%s:%s:domain/%s", region, account, name)
}

func domainEndpoint(name, region string) string {
	return fmt.Sprintf("search-%s-%s.%s.es.amazonaws.com", name, common.RandomID(28), region)
}
