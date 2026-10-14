package codeartifact

import (
	"context"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/codeartifact"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	// A repository has at most 10 direct upstream repositories, from the CodeArtifact quotas.
	maxUpstreams = 10

	connectionAvailable = "Available"
	formatMaven         = "maven"
)

// externalConnections are the public repositories a repository can connect to, with their package formats, from
// the CodeArtifact guide on external connections.
var externalConnections = map[string]string{
	"public:npmjs":               "npm",
	"public:pypi":                "pypi",
	"public:maven-central":       formatMaven,
	"public:maven-googleandroid": formatMaven,
	"public:maven-gradleplugins": formatMaven,
	"public:maven-commonsware":   formatMaven,
	"public:nuget-org":           "nuget",
	"public:ruby-gems-org":       "ruby",
	"public:crates-io":           "cargo",
}

type repositoryRecord struct {
	tagSet
	Created     time.Time
	Policy      *policy
	Domain      string
	Name        string
	Description string
	Upstreams   []string
	External    []string
}

func (r *repositoryRecord) description(scope request.Scope) *api.RepositoryDescription {
	out := &api.RepositoryDescription{
		Name:                 new(r.Name),
		AdministratorAccount: new(scope.AccountID),
		DomainName:           new(r.Domain),
		DomainOwner:          new(scope.AccountID),
		Arn:                  new(repositoryARN(scope, r.Domain, r.Name)),
		Description:          optional(r.Description),
		Upstreams:            []api.UpstreamRepositoryInfo{},
		ExternalConnections:  []api.RepositoryExternalConnectionInfo{},
		CreatedTime:          common.Seconds(r.Created),
	}
	for _, u := range r.Upstreams {
		out.Upstreams = append(out.Upstreams, api.UpstreamRepositoryInfo{RepositoryName: new(u)})
	}

	for _, e := range r.External {
		out.ExternalConnections = append(out.ExternalConnections, api.RepositoryExternalConnectionInfo{
			ExternalConnectionName: new(
				e,
			),
			PackageFormat: new(externalConnections[e]),
			Status:        new(connectionAvailable),
		})
	}

	return out
}

func (r *repositoryRecord) summary(scope request.Scope) api.RepositorySummary {
	return api.RepositorySummary{
		Name:                 new(r.Name),
		AdministratorAccount: new(scope.AccountID),
		DomainName:           new(r.Domain),
		DomainOwner:          new(scope.AccountID),
		Arn:                  new(repositoryARN(scope, r.Domain, r.Name)),
		Description:          optional(r.Description),
		CreatedTime:          common.Seconds(r.Created),
	}
}

// optional returns nil for an empty value, so unset members stay out of responses.
func optional(v string) *string {
	if v == "" {
		return nil
	}

	return &v
}

// upstreamNames validates the upstreams of a repository: each exists in the domain, none is the repository itself
// or repeated, and no cycle forms through the other repositories' upstreams.
func upstreamNames(
	tx storage.ReadTx,
	scope request.Scope,
	domain, repository string,
	in []api.UpstreamRepository,
) ([]string, error) {
	if len(in) > maxUpstreams {
		return nil, fault(
			"ServiceQuotaExceededException",
			"A repository can have at most %d upstream repositories.",
			maxUpstreams,
		)
	}

	names := make([]string, 0, len(in))
	for _, u := range in {
		name, err := checkRepository(u.RepositoryName)
		if err != nil {
			return nil, err
		}

		if name == repository {
			return nil, invalid("A repository cannot be its own upstream.")
		}

		if slices.Contains(names, name) {
			return nil, invalid("Upstream repository %s is listed more than once.", name)
		}

		if _, found := tx.Get(repositoryKey(scope, domain, name)); !found {
			return nil, repositoryNotFound(domain, name)
		}

		names = append(names, name)
	}

	for _, name := range names {
		reaches, err := reachable(tx, scope, domain, name, repository, map[string]bool{})
		if err != nil {
			return nil, err
		}

		if reaches {
			return nil, invalid("Upstream repository %s would make the upstream chain circular.", name)
		}
	}

	return names, nil
}

// reachable reports whether target can be reached from the repository from by following upstreams.
func reachable(
	tx storage.ReadTx,
	scope request.Scope,
	domain, from, target string,
	seen map[string]bool,
) (bool, error) {
	if from == target {
		return true, nil
	}

	if seen[from] {
		return false, nil
	}

	seen[from] = true
	r, found, err := storage.Read[repositoryRecord](tx, repositoryKey(scope, domain, from))
	if err != nil || !found {
		return false, err
	}

	for _, u := range r.Upstreams {
		reaches, e := reachable(tx, scope, domain, u, target, seen)
		if e != nil || reaches {
			return reaches, e
		}
	}

	return false, nil
}

func (s *Service) CreateRepository(
	ctx context.Context,
	in *api.CreateRepositoryRequest,
) (*api.CreateRepositoryResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	domain, err := checkDomain(scope, in.Domain, in.DomainOwner)
	if err != nil {
		return nil, err
	}

	name, err := checkRepository(in.Repository)
	if err != nil {
		return nil, err
	}

	if descErr := checkDescription(in.Description); descErr != nil {
		return nil, descErr
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	r := &repositoryRecord{
		Domain: domain, Name: name, Description: common.Value(in.Description), Created: time.Now().UTC(),
		tagSet: tagSet{Tags: tags},
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found := tx.Get(domainKey(scope, domain)); !found {
			return domainNotFound(domain)
		}

		if _, exists := tx.Get(repositoryKey(scope, domain, name)); exists {
			return fault("ConflictException", "Repository %s already exists in domain %s.", name, domain)
		}

		var e error
		if r.Upstreams, e = upstreamNames(tx, scope, domain, name, in.Upstreams); e != nil {
			return e
		}

		return storage.Write(tx, repositoryKey(scope, domain, name), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateRepositoryResult{Repository: r.description(scope)}, nil
}

// readRepository reads a repository in the caller's transaction.
func readRepository(tx storage.ReadTx, scope request.Scope, domain, name string) (*repositoryRecord, error) {
	if _, found := tx.Get(domainKey(scope, domain)); !found {
		return nil, domainNotFound(domain)
	}

	r, found, err := storage.Read[repositoryRecord](tx, repositoryKey(scope, domain, name))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, repositoryNotFound(domain, name)
	}

	return r, nil
}

// target resolves the domain and repository named by a request.
func target(scope request.Scope, domain, owner, repository *string) (d, r string, err error) {
	if d, err = checkDomain(scope, domain, owner); err != nil {
		return "", "", err
	}

	if r, err = checkRepository(repository); err != nil {
		return "", "", err
	}

	return d, r, nil
}

func (s *Service) DescribeRepository(
	ctx context.Context,
	in *api.DescribeRepositoryRequest,
) (*api.DescribeRepositoryResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	domain, name, err := target(scope, in.Domain, in.DomainOwner, in.Repository)
	if err != nil {
		return nil, err
	}

	var out *api.RepositoryDescription
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := readRepository(tx, scope, domain, name)
		if e != nil {
			return e
		}

		out = r.description(scope)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeRepositoryResult{Repository: out}, nil
}

// change updates a repository in one transaction and returns its description.
func (s *Service) change(
	ctx context.Context,
	domain, owner, repository *string,
	apply func(tx storage.WriteTx, scope request.Scope, r *repositoryRecord) error,
) (*api.RepositoryDescription, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	d, name, err := target(scope, domain, owner, repository)
	if err != nil {
		return nil, err
	}

	var out *api.RepositoryDescription
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readRepository(tx, scope, d, name)
		if e != nil {
			return e
		}

		if applyErr := apply(tx, scope, r); applyErr != nil {
			return applyErr
		}

		out = r.description(scope)

		return storage.Write(tx, repositoryKey(scope, d, name), r)
	})

	return out, err
}

func (s *Service) UpdateRepository(
	ctx context.Context,
	in *api.UpdateRepositoryRequest,
) (*api.UpdateRepositoryResult, error) {
	if in.Description == nil && in.Upstreams == nil {
		return nil, invalid("description or upstreams is required.")
	}

	if err := checkDescription(in.Description); err != nil {
		return nil, err
	}

	out, err := s.change(ctx, in.Domain, in.DomainOwner, in.Repository,
		func(tx storage.WriteTx, scope request.Scope, r *repositoryRecord) error {
			if in.Description != nil {
				r.Description = *in.Description
			}

			if in.Upstreams != nil {
				var e error
				if r.Upstreams, e = upstreamNames(tx, scope, r.Domain, r.Name, in.Upstreams); e != nil {
					return e
				}
			}

			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.UpdateRepositoryResult{Repository: out}, nil
}

func (s *Service) AssociateExternalConnection(
	ctx context.Context,
	in *api.AssociateExternalConnectionRequest,
) (*api.AssociateExternalConnectionResult, error) {
	connection := common.Value(in.ExternalConnection)
	if _, known := externalConnections[connection]; !known {
		return nil, invalid("externalConnection %q is not a supported public repository.", connection)
	}

	out, err := s.change(ctx, in.Domain, in.DomainOwner, in.Repository,
		func(_ storage.WriteTx, _ request.Scope, r *repositoryRecord) error {
			if slices.Contains(r.External, connection) {
				return fault("ConflictException", "Repository %s is already connected to %s.", r.Name, connection)
			}

			if len(r.External) > 0 {
				return fault("ConflictException", "Repository %s already has an external connection.", r.Name)
			}

			r.External = append(r.External, connection)

			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.AssociateExternalConnectionResult{Repository: out}, nil
}

func (s *Service) DisassociateExternalConnection(
	ctx context.Context,
	in *api.DisassociateExternalConnectionRequest,
) (*api.DisassociateExternalConnectionResult, error) {
	connection := common.Value(in.ExternalConnection)
	if _, known := externalConnections[connection]; !known {
		return nil, invalid("externalConnection %q is not a supported public repository.", connection)
	}

	out, err := s.change(ctx, in.Domain, in.DomainOwner, in.Repository,
		func(_ storage.WriteTx, _ request.Scope, r *repositoryRecord) error {
			i := slices.Index(r.External, connection)
			if i < 0 {
				return fault(
					"ResourceNotFoundException",
					"Repository %s has no external connection %s.",
					r.Name,
					connection,
				)
			}

			r.External = slices.Delete(r.External, i, i+1)

			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.DisassociateExternalConnectionResult{Repository: out}, nil
}

func (s *Service) DeleteRepository(
	ctx context.Context,
	in *api.DeleteRepositoryRequest,
) (*api.DeleteRepositoryResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	domain, name, err := target(scope, in.Domain, in.DomainOwner, in.Repository)
	if err != nil {
		return nil, err
	}

	var out *api.RepositoryDescription
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readRepository(tx, scope, domain, name)
		if e != nil {
			return e
		}

		// Another repository's upstream list would point at nothing, so an upstream cannot be deleted.
		others, e := storage.Records[repositoryRecord](tx, repositoryKey(scope, domain, ""))
		if e != nil {
			return e
		}

		if slices.ContainsFunc(others, func(o *repositoryRecord) bool { return slices.Contains(o.Upstreams, name) }) {
			return fault("ConflictException", "Repository %s is an upstream of another repository.", name)
		}

		out = r.description(scope)
		tx.Delete(repositoryKey(scope, domain, name))

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteRepositoryResult{Repository: out}, nil
}

// listRepositories returns the repositories under a key prefix whose names start with the given prefix.
func (s *Service) listRepositories(
	ctx context.Context,
	domain, owner *string,
	administrator, prefix, token *string,
	maxResults *int64,
) ([]api.RepositorySummary, *string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	limit, err := pageSize(maxResults)
	if err != nil {
		return nil, nil, err
	}

	keyPrefix := scope.Prefix(serviceName, kindRepository)
	if domain != nil {
		d, e := checkDomain(scope, domain, owner)
		if e != nil {
			return nil, nil, e
		}

		keyPrefix += d + "/"
	}

	var items []*repositoryRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if domain != nil {
			if _, found := tx.Get(domainKey(scope, *domain)); !found {
				return domainNotFound(*domain)
			}
		}

		all, e := storage.Records[repositoryRecord](tx, keyPrefix)
		for _, r := range all {
			if prefix == nil || strings.HasPrefix(r.Name, *prefix) {
				items = append(items, r)
			}
		}

		return e
	})
	if err != nil {
		return nil, nil, err
	}

	// Every repository is administered by the account that owns it.
	if administrator != nil && *administrator != scope.AccountID {
		items = nil
	}

	page, next, err := common.Page(
		items, func(r *repositoryRecord) string { return r.Domain + "/" + r.Name }, token, limit, invalidToken(),
	)
	if err != nil {
		return nil, nil, err
	}

	out := make([]api.RepositorySummary, 0, len(page))
	for _, r := range page {
		out = append(out, r.summary(scope))
	}

	return out, next, nil
}

func (s *Service) ListRepositories(
	ctx context.Context,
	in *api.ListRepositoriesRequest,
) (*api.ListRepositoriesResult, error) {
	repositories, next, err := s.listRepositories(ctx, nil, nil, nil, in.RepositoryPrefix, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	return &api.ListRepositoriesResult{Repositories: repositories, NextToken: next}, nil
}

func (s *Service) ListRepositoriesInDomain(
	ctx context.Context,
	in *api.ListRepositoriesInDomainRequest,
) (*api.ListRepositoriesInDomainResult, error) {
	if in.Domain == nil {
		return nil, invalid("domain is required.")
	}

	repositories, next, err := s.listRepositories(
		ctx, in.Domain, in.DomainOwner, in.AdministratorAccount, in.RepositoryPrefix, in.NextToken, in.MaxResults,
	)
	if err != nil {
		return nil, err
	}

	return &api.ListRepositoriesInDomainResult{Repositories: repositories, NextToken: next}, nil
}
