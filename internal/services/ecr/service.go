// Package ecr implements Amazon Elastic Container Registry without HTTP
// dependencies: the ECR API and the operations the Docker Registry HTTP API
// needs, sharing one store. Repositories belong to an account and region:
//
//	<scope>/ecr/repo/<name>
//	<scope>/ecr/image/<escaped name>/<digest>  (manifest, tags, push and pull times)
//	<scope>/ecr/tag/<escaped name>/<tag>       (a digest)
//	<scope>/ecr/layer/<escaped name>/<digest>  (size)
//	<scope>/ecr/blob/<escaped name>/<digest>   (bytes)
//	<scope>/ecr/upload/<escaped name>/<upload ID> and upload-data/…
//
// Repository names may contain "/", so the records a repository owns use its
// path-escaped name, and one repository's prefix never lists another's.
// Authorization tokens are partition-wide, aws/ecr/token/<SHA-256 of the
// password>, because a registry client presents only the password.
package ecr

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecr"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service = "ecr"
	// maxPage is the default page size of the list operations.
	maxPage = 100
)

var _ api.API = (*Service)(nil)

type Service struct {
	store storage.Store
	now   func() time.Time
}

func New(store storage.Store) *Service {
	return &Service{store: store, now: time.Now}
}

func invalidParameter(format string, args ...any) error {
	return awserror.New("InvalidParameterException", fmt.Sprintf(format, args...))
}

func repositoryNotFound(scope request.Scope, name string) error {
	return awserror.New("RepositoryNotFoundException", fmt.Sprintf(
		"The repository with name '%s' does not exist in the registry with id '%s'", name, scope.AccountID))
}

func identity(ctx context.Context) (request.Metadata, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" || m.Region == "" {
		return request.Metadata{}, fmt.Errorf("missing request identity")
	}

	return m, nil
}

func prefix(scope request.Scope, kind string) string {
	return scope.AccountID + "/" + scope.Region + "/" + service + "/" + kind + "/"
}

// repoKinds are the record kinds a repository owns, each under
// <kind>/<escaped name>/.
var repoKinds = []string{"image", "tag", "layer", "blob", "upload", "upload-data"}

func repoKey(scope request.Scope, name string) string { return prefix(scope, "repo") + name }

// owned is the prefix of a repository's records of one kind.
func owned(scope request.Scope, kind, repo string) string {
	return prefix(scope, kind) + url.PathEscape(repo) + "/"
}

func repoARN(scope request.Scope, name string) string {
	return "arn:aws:ecr:" + scope.Region + ":" + scope.AccountID + ":repository/" + name
}

// registryHost is the host a repository's URI names: the endpoint the client
// used, so Docker pushes to TinyStack.
func registryHost(m *request.Metadata) string {
	return strings.TrimPrefix(strings.TrimPrefix(m.Endpoint, "http://"), "https://")
}

func seconds(t time.Time) *float64 { return common.SecondsMilli(t) }

func value[T any](p *T) (v T) { return common.Value(p) }

func read[T any](tx storage.ReadTx, k string) (v *T, found bool, err error) {
	return storage.Read[T](tx, k)
}

func write(tx storage.WriteTx, k string, v any) error { return storage.Write(tx, k, v) }

// page returns the items after the one token names, at most limit of them, and
// the token for the rest, if any. Tokens are the last returned item's ID.
func page[T any](items []T, id func(T) string, token *string, limit int) (selected []T, next *string, err error) {
	start := 0
	if token != nil {
		after, decodeErr := base64.RawURLEncoding.DecodeString(*token)
		if decodeErr != nil || len(after) == 0 {
			return nil, nil, invalidParameter("Invalid parameter at 'nextToken' failed to satisfy constraint: " +
				"'Invalid pagination token'")
		}

		start = len(items)
		for i, item := range items {
			if id(item) > string(after) {
				start = i
				break
			}
		}
	}

	items = items[start:]
	if len(items) <= limit {
		return items, nil, nil
	}

	next = new(base64.RawURLEncoding.EncodeToString([]byte(id(items[limit-1]))))
	return items[:limit], next, nil
}

// limit validates an optional page size of 1 to 1000, defaulting to maxPage.
func limit(n *int64) (int, error) {
	if n == nil {
		return maxPage, nil
	}

	if *n < 1 || *n > 1000 {
		return 0, invalidParameter("Invalid parameter at 'maxResults' failed to satisfy constraint: " +
			"'Member must have value between 1 and 1000, inclusive'")
	}

	return int(*n), nil
}

var repoName = regexp.MustCompile(`^(?:[a-z0-9]+(?:[._-][a-z0-9]+)*/)*[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

func checkRepoName(name *string) error {
	if name == nil || len(*name) < 2 || len(*name) > 256 || !repoName.MatchString(*name) {
		return invalidParameter("Invalid parameter at 'repositoryName' failed to satisfy constraint: 'must satisfy "+
			"regular expression '(?:[a-z0-9]+(?:[._-][a-z0-9]+)*/)*[a-z0-9]+(?:[._-][a-z0-9]+)*' and be 2 to 256 "+
			"characters long' (%q)", value(name))
	}

	return nil
}

// checkRegistry accepts only the caller's own registry.
func checkRegistry(scope request.Scope, id *string) error {
	if id != nil && *id != scope.AccountID {
		return invalidParameter("TinyStack serves only the caller's registry %s, not %s.", scope.AccountID, *id)
	}

	return nil
}

// loadRepo returns the repository name names in scope.
func loadRepo(tx storage.ReadTx, scope request.Scope, registry, name *string) (*repository, error) {
	if err := checkRegistry(scope, registry); err != nil {
		return nil, err
	}

	if err := checkRepoName(name); err != nil {
		return nil, err
	}

	r, ok, err := read[repository](tx, repoKey(scope, *name))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, repositoryNotFound(scope, *name)
	}

	return r, nil
}

// view runs fn with the repository the request names.
func (s *Service) view(
	ctx context.Context,
	registry, name *string,
	fn func(storage.ReadTx, *request.Metadata, *repository) error,
) error {
	m, err := identity(ctx)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error {
		r, loadErr := loadRepo(tx, m.Scope, registry, name)
		if loadErr != nil {
			return loadErr
		}

		return fn(tx, &m, r)
	})
}

// update runs fn with the repository the request names and stores the
// repository after it.
func (s *Service) update(
	ctx context.Context,
	registry, name *string,
	fn func(storage.WriteTx, *request.Metadata, *repository) error,
) error {
	m, err := identity(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, loadErr := loadRepo(tx, m.Scope, registry, name)
		if loadErr != nil {
			return loadErr
		}

		if fnErr := fn(tx, &m, r); fnErr != nil {
			return fnErr
		}

		return write(tx, repoKey(m.Scope, r.Name), r)
	})
}

// suffixes returns the rest of each key under keyPrefix, in order.
func suffixes(tx storage.ReadTx, keyPrefix string) []string {
	keys := tx.Keys(keyPrefix)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, strings.TrimPrefix(k, keyPrefix))
	}

	return out
}
