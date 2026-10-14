package codeartifact

import (
	"context"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/codeartifact"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// policy is a resource permissions policy and the revision that identifies its current version.
type policy struct {
	Revision string
	Document string
}

func (p *policy) output(arn string) *api.ResourcePolicy {
	return &api.ResourcePolicy{ResourceArn: new(arn), Revision: new(p.Revision), Document: new(p.Document)}
}

type domainRecord struct {
	tagSet
	Created       time.Time
	Policy        *policy
	Name          string
	EncryptionKey string
}

func (r *domainRecord) description(scope request.Scope, repositories int) *api.DomainDescription {
	return &api.DomainDescription{
		Name:            new(r.Name),
		Owner:           new(scope.AccountID),
		Arn:             new(domainARN(scope, r.Name)),
		Status:          new(statusActive),
		CreatedTime:     common.Seconds(r.Created),
		EncryptionKey:   new(r.EncryptionKey),
		RepositoryCount: new(int64(repositories)),
		AssetSizeBytes:  new(int64(0)),
	}
}

func (r *domainRecord) summary(scope request.Scope) api.DomainSummary {
	return api.DomainSummary{
		Name:          new(r.Name),
		Owner:         new(scope.AccountID),
		Arn:           new(domainARN(scope, r.Name)),
		Status:        new(statusActive),
		CreatedTime:   common.Seconds(r.Created),
		EncryptionKey: new(r.EncryptionKey),
	}
}

// repositoryCount counts the repositories of a domain.
func repositoryCount(tx storage.ReadTx, scope request.Scope, domain string) int {
	return len(tx.Keys(repositoryKey(scope, domain, "")))
}

func (s *Service) CreateDomain(ctx context.Context, in *api.CreateDomainRequest) (*api.CreateDomainResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	domain, err := checkDomain(scope, in.Domain, nil)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	key := "arn:aws:kms:" + scope.Region + ":" + scope.AccountID + ":key/" + common.RandomID(8) + "-" +
		common.RandomID(4) + "-" + common.RandomID(4) + "-" + common.RandomID(4) + "-" + common.RandomID(12)
	if in.EncryptionKey != nil {
		if !strings.HasPrefix(*in.EncryptionKey, "arn:") || len(*in.EncryptionKey) > 1011 {
			return nil, invalid("encryptionKey must be a KMS key ARN.")
		}

		key = *in.EncryptionKey
	}

	r := &domainRecord{Name: domain, Created: time.Now().UTC(), EncryptionKey: key, tagSet: tagSet{Tags: tags}}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, exists := tx.Get(domainKey(scope, domain)); exists {
			return fault("ConflictException", "Domain %s already exists.", domain)
		}

		return storage.Write(tx, domainKey(scope, domain), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateDomainResult{Domain: r.description(scope, 0)}, nil
}

func (s *Service) DescribeDomain(
	ctx context.Context,
	in *api.DescribeDomainRequest,
) (*api.DescribeDomainResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	domain, err := checkDomain(scope, in.Domain, in.DomainOwner)
	if err != nil {
		return nil, err
	}

	var out *api.DomainDescription
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, e := storage.Read[domainRecord](tx, domainKey(scope, domain))
		if e != nil {
			return e
		}

		if !found {
			return domainNotFound(domain)
		}

		out = r.description(scope, repositoryCount(tx, scope, domain))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeDomainResult{Domain: out}, nil
}

func (s *Service) DeleteDomain(ctx context.Context, in *api.DeleteDomainRequest) (*api.DeleteDomainResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	domain, err := checkDomain(scope, in.Domain, in.DomainOwner)
	if err != nil {
		return nil, err
	}

	var out *api.DomainDescription
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, e := storage.Read[domainRecord](tx, domainKey(scope, domain))
		if e != nil {
			return e
		}

		if !found {
			return domainNotFound(domain)
		}

		if repositoryCount(tx, scope, domain) > 0 {
			return fault("ConflictException", "Domain %s still has repositories; delete them first.", domain)
		}

		out = r.description(scope, 0)
		out.Status = new("Deleted")
		tx.Delete(domainKey(scope, domain))

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteDomainResult{Domain: out}, nil
}

func (s *Service) ListDomains(ctx context.Context, in *api.ListDomainsRequest) (*api.ListDomainsResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	var items []*domainRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		items, e = storage.Records[domainRecord](tx, scope.Prefix(serviceName, kindDomain))
		return e
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		items, func(r *domainRecord) string { return r.Name }, in.NextToken, limit, invalidToken(),
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListDomainsResult{NextToken: next, Domains: []api.DomainSummary{}}
	for _, r := range page {
		out.Domains = append(out.Domains, r.summary(scope))
	}

	return out, nil
}
