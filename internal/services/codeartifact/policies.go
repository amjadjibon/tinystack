package codeartifact

import (
	"context"
	"encoding/json"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/codeartifact"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// maxPolicy is the model's limit on a policy document.
const maxPolicy = 7168

// checkDocument validates that a policy document is a JSON object with a Statement, the only structure that
// TinyStack checks; principals, actions and conditions are not evaluated.
func checkDocument(document *string) (string, error) {
	if document == nil || *document == "" || len(*document) > maxPolicy {
		return "", invalid("policyDocument must be 1 to %d characters long.", maxPolicy)
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(*document), &parsed); err != nil {
		return "", fault("MalformedPolicyException", "The policy document is not valid JSON: %v", err)
	}

	if _, ok := parsed["Statement"]; !ok {
		return "", fault("MalformedPolicyException", "The policy document needs a Statement.")
	}

	return *document, nil
}

func staleRevision(have string) error {
	return fault("ConflictException", "The policy revision does not match the current revision %s.", have)
}

// put replaces the policy a resource holds. A given revision must match the current one.
func put(current *policy, revision *string, document string) (*policy, error) {
	if revision != nil && (current == nil || current.Revision != *revision) {
		return nil, staleRevision(common.Value(revisionOf(current)))
	}

	return &policy{Revision: common.RandomID(32), Document: document}, nil
}

func revisionOf(p *policy) *string {
	if p == nil {
		return nil
	}

	return &p.Revision
}

// existing checks that a resource has a policy, and that a given revision is its current one.
func existing(current *policy, revision *string, arn string) error {
	if current == nil {
		return fault("ResourceNotFoundException", "%s has no permissions policy.", arn)
	}

	if revision != nil && current.Revision != *revision {
		return staleRevision(current.Revision)
	}

	return nil
}

func (s *Service) PutDomainPermissionsPolicy(
	ctx context.Context,
	in *api.PutDomainPermissionsPolicyRequest,
) (*api.PutDomainPermissionsPolicyResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	domain, err := checkDomain(scope, in.Domain, in.DomainOwner)
	if err != nil {
		return nil, err
	}

	document, err := checkDocument(in.PolicyDocument)
	if err != nil {
		return nil, err
	}

	var out *policy
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, e := storage.Read[domainRecord](tx, domainKey(scope, domain))
		if e != nil {
			return e
		}

		if !found {
			return domainNotFound(domain)
		}

		if out, e = put(r.Policy, in.PolicyRevision, document); e != nil {
			return e
		}

		r.Policy = out

		return storage.Write(tx, domainKey(scope, domain), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.PutDomainPermissionsPolicyResult{Policy: out.output(domainARN(scope, domain))}, nil
}

func (s *Service) GetDomainPermissionsPolicy(
	ctx context.Context,
	in *api.GetDomainPermissionsPolicyRequest,
) (*api.GetDomainPermissionsPolicyResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	domain, err := checkDomain(scope, in.Domain, in.DomainOwner)
	if err != nil {
		return nil, err
	}

	var out *policy
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, e := storage.Read[domainRecord](tx, domainKey(scope, domain))
		if e != nil {
			return e
		}

		if !found {
			return domainNotFound(domain)
		}

		if pe := existing(r.Policy, nil, domainARN(scope, domain)); pe != nil {
			return pe
		}

		out = r.Policy

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetDomainPermissionsPolicyResult{Policy: out.output(domainARN(scope, domain))}, nil
}

func (s *Service) DeleteDomainPermissionsPolicy(
	ctx context.Context,
	in *api.DeleteDomainPermissionsPolicyRequest,
) (*api.DeleteDomainPermissionsPolicyResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	domain, err := checkDomain(scope, in.Domain, in.DomainOwner)
	if err != nil {
		return nil, err
	}

	var out *policy
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, e := storage.Read[domainRecord](tx, domainKey(scope, domain))
		if e != nil {
			return e
		}

		if !found {
			return domainNotFound(domain)
		}

		if pe := existing(r.Policy, in.PolicyRevision, domainARN(scope, domain)); pe != nil {
			return pe
		}

		out, r.Policy = r.Policy, nil

		return storage.Write(tx, domainKey(scope, domain), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteDomainPermissionsPolicyResult{Policy: out.output(domainARN(scope, domain))}, nil
}

func (s *Service) PutRepositoryPermissionsPolicy(
	ctx context.Context,
	in *api.PutRepositoryPermissionsPolicyRequest,
) (*api.PutRepositoryPermissionsPolicyResult, error) {
	document, err := checkDocument(in.PolicyDocument)
	if err != nil {
		return nil, err
	}

	var out *policy
	var arn string
	_, err = s.change(ctx, in.Domain, in.DomainOwner, in.Repository,
		func(_ storage.WriteTx, scope request.Scope, r *repositoryRecord) error {
			var e error
			if out, e = put(r.Policy, in.PolicyRevision, document); e != nil {
				return e
			}

			r.Policy, arn = out, repositoryARN(scope, r.Domain, r.Name)

			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.PutRepositoryPermissionsPolicyResult{Policy: out.output(arn)}, nil
}

func (s *Service) GetRepositoryPermissionsPolicy(
	ctx context.Context,
	in *api.GetRepositoryPermissionsPolicyRequest,
) (*api.GetRepositoryPermissionsPolicyResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	domain, name, err := target(scope, in.Domain, in.DomainOwner, in.Repository)
	if err != nil {
		return nil, err
	}

	var out *policy
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := readRepository(tx, scope, domain, name)
		if e != nil {
			return e
		}

		if pe := existing(r.Policy, nil, repositoryARN(scope, domain, name)); pe != nil {
			return pe
		}

		out = r.Policy

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetRepositoryPermissionsPolicyResult{Policy: out.output(repositoryARN(scope, domain, name))}, nil
}

func (s *Service) DeleteRepositoryPermissionsPolicy(
	ctx context.Context,
	in *api.DeleteRepositoryPermissionsPolicyRequest,
) (*api.DeleteRepositoryPermissionsPolicyResult, error) {
	var out *policy
	var arn string
	_, err := s.change(ctx, in.Domain, in.DomainOwner, in.Repository,
		func(_ storage.WriteTx, scope request.Scope, r *repositoryRecord) error {
			arn = repositoryARN(scope, r.Domain, r.Name)
			if e := existing(r.Policy, in.PolicyRevision, arn); e != nil {
				return e
			}

			out, r.Policy = r.Policy, nil

			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.DeleteRepositoryPermissionsPolicyResult{Policy: out.output(arn)}, nil
}
