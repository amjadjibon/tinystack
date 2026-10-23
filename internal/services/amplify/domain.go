package amplify

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/amplify"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

// CreateDomainAssociation serves the corresponding records-only Hosting operation.
func (s *Service) CreateDomainAssociation(
	ctx context.Context,
	in *api.CreateDomainAssociationRequest,
) (*api.CreateDomainAssociationResult, error) {
	if err := validateCreateDomainAssociationRequest(in); err != nil {
		return nil, err
	}

	id := childID(common.Value(in.AppId), common.Value(in.DomainName))
	r, err := s.change(
		ctx,
		"domain",
		id,
		common.Value(in.AppId),
		"",
		true,
		false,
		func(r *record, scope request.Scope) error {
			r.Domain = &api.DomainAssociation{}
			r.Name = common.Value(in.DomainName)
			r.ARN = resourceARN(scope, "apps/"+common.Value(in.AppId)+"/domains/"+r.Name)
			r.Domain.DomainAssociationArn = new(r.ARN)
			r.Domain.DomainStatus = new("PENDING_VERIFICATION")
			r.Domain.StatusReason = new("Records only: DNS and certificate verification are not performed.")
			r.Domain.EnableAutoSubDomain = new(false)
			r.Domain.SubDomains = []api.SubDomain{}
			if in.DomainName != nil {
				r.Domain.DomainName = in.DomainName
			}

			if in.EnableAutoSubDomain != nil {
				r.Domain.EnableAutoSubDomain = in.EnableAutoSubDomain
			}

			if in.AutoSubDomainCreationPatterns != nil {
				r.Domain.AutoSubDomainCreationPatterns = in.AutoSubDomainCreationPatterns
			}

			if in.AutoSubDomainIAMRole != nil {
				r.Domain.AutoSubDomainIAMRole = in.AutoSubDomainIAMRole
			}

			if in.SubDomainSettings != nil {
				r.Domain.SubDomains = []api.SubDomain{}
				for _, setting := range in.SubDomainSettings {
					r.Domain.SubDomains = append(
						r.Domain.SubDomains,
						api.SubDomain{
							SubDomainSetting: common.Clone(new(setting)),
							Verified:         new(false),
							DnsRecord:        new(""),
						},
					)
				}
			}

			if in.CertificateSettings != nil {
				r.Domain.Certificate = &api.Certificate{
					Type:                 in.CertificateSettings.Type,
					CustomCertificateArn: in.CertificateSettings.CustomCertificateArn,
				}
			}

			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.CreateDomainAssociationResult{DomainAssociation: common.Clone(r.Domain)}, nil
}

// GetDomainAssociation serves the corresponding records-only Hosting operation.
func (s *Service) GetDomainAssociation(
	ctx context.Context,
	in *api.GetDomainAssociationRequest,
) (*api.GetDomainAssociationResult, error) {
	if err := validateGetDomainAssociationRequest(in); err != nil {
		return nil, err
	}

	id := childID(common.Value(in.AppId), common.Value(in.DomainName))
	r, err := s.get(ctx, "domain", id)
	if err != nil {
		return nil, err
	}

	return &api.GetDomainAssociationResult{DomainAssociation: common.Clone(r.Domain)}, nil
}

// UpdateDomainAssociation serves the corresponding records-only Hosting operation.
func (s *Service) UpdateDomainAssociation(
	ctx context.Context,
	in *api.UpdateDomainAssociationRequest,
) (*api.UpdateDomainAssociationResult, error) {
	if err := validateUpdateDomainAssociationRequest(in); err != nil {
		return nil, err
	}

	id := childID(common.Value(in.AppId), common.Value(in.DomainName))
	r, err := s.change(
		ctx,
		"domain",
		id,
		common.Value(in.AppId),
		"",
		false,
		false,
		func(r *record, scope request.Scope) error {
			if in.DomainName != nil {
				r.Domain.DomainName = in.DomainName
			}

			if in.EnableAutoSubDomain != nil {
				r.Domain.EnableAutoSubDomain = in.EnableAutoSubDomain
			}

			if in.AutoSubDomainCreationPatterns != nil {
				r.Domain.AutoSubDomainCreationPatterns = in.AutoSubDomainCreationPatterns
			}

			if in.AutoSubDomainIAMRole != nil {
				r.Domain.AutoSubDomainIAMRole = in.AutoSubDomainIAMRole
			}

			if in.SubDomainSettings != nil {
				r.Domain.SubDomains = []api.SubDomain{}
				for _, setting := range in.SubDomainSettings {
					r.Domain.SubDomains = append(
						r.Domain.SubDomains,
						api.SubDomain{
							SubDomainSetting: common.Clone(new(setting)),
							Verified:         new(false),
							DnsRecord:        new(""),
						},
					)
				}
			}

			if in.CertificateSettings != nil {
				r.Domain.Certificate = &api.Certificate{
					Type:                 in.CertificateSettings.Type,
					CustomCertificateArn: in.CertificateSettings.CustomCertificateArn,
				}
			}

			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.UpdateDomainAssociationResult{DomainAssociation: common.Clone(r.Domain)}, nil
}

// DeleteDomainAssociation serves the corresponding records-only Hosting operation.
func (s *Service) DeleteDomainAssociation(
	ctx context.Context,
	in *api.DeleteDomainAssociationRequest,
) (*api.DeleteDomainAssociationResult, error) {
	if err := validateDeleteDomainAssociationRequest(in); err != nil {
		return nil, err
	}

	id := childID(common.Value(in.AppId), common.Value(in.DomainName))
	r, err := s.change(
		ctx,
		"domain",
		id,
		common.Value(in.AppId),
		"",
		false,
		true,
		func(r *record, scope request.Scope) error {
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.DeleteDomainAssociationResult{DomainAssociation: common.Clone(r.Domain)}, nil
}

// ListDomainAssociations serves the corresponding records-only Hosting operation.
func (s *Service) ListDomainAssociations(
	ctx context.Context,
	in *api.ListDomainAssociationsRequest,
) (*api.ListDomainAssociationsResult, error) {
	if err := validateListDomainAssociationsRequest(in); err != nil {
		return nil, err
	}

	items, next, err := s.list(ctx, "domain", common.Value(in.AppId), "", "", in.NextToken, in.MaxResults, 50)
	if err != nil {
		return nil, err
	}

	out := &api.ListDomainAssociationsResult{NextToken: next, DomainAssociations: []api.DomainAssociation{}}
	for _, r := range items {
		out.DomainAssociations = append(out.DomainAssociations, *common.Clone(r.Domain))
	}

	return out, nil
}
