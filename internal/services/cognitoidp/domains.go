package cognitoidp

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"regexp"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var certificateARN = regexp.MustCompile(
	`^arn:[\w+=/,.@-]+:[\w+=/,.@-]+:([\w+=/,.@-]*)?:[0-9]+:[\w+=/,.@-]+(:[\w+=/,.@-]+)?(:[\w+=/,.@-]+)?$`,
)

var domainPrefix = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9\-]{0,61}[a-z0-9])?$`)

type domain struct {
	Description api.DomainDescriptionType
	Scope       request.Scope
}

func domainKey(scope request.Scope, pool string) string { return scope.Key(service, "domain", pool) }

func domainIndexKey(host string) string { return "aws/" + service + "/domain/" + host }

func (d *domain) host() string {
	name := common.Value(d.Description.Domain)
	if d.Description.CustomDomainConfig != nil {
		return name
	}

	return name + ".auth." + d.Scope.Region + ".amazoncognito.com"
}

func checkDomain(name *string, config *api.CustomDomainConfigType, version *int64) error {
	if version != nil && *version != 1 && *version != 2 {
		return invalidParameter("ManagedLoginVersion must be 1 or 2.")
	}

	if name == nil || len(*name) > 63 {
		return invalidParameter("Domain must contain 1 to 63 characters.")
	}

	if config == nil {
		if !domainPrefix.MatchString(*name) {
			return invalidParameter("Invalid domain prefix.")
		}

		for _, reserved := range []string{"aws", "amazon", "cognito"} {
			if strings.Contains(*name, reserved) {
				return invalidParameter("Domain contains a reserved term.")
			}
		}

		return nil
	}

	if !strings.Contains(*name, ".") {
		return invalidParameter("Custom domain must be a fully-qualified domain name.")
	}

	for _, label := range strings.Split(*name, ".") {
		if !domainPrefix.MatchString(label) {
			return invalidParameter("Invalid custom domain.")
		}
	}

	certificate := common.Value(config.CertificateArn)
	if len(certificate) < 20 || len(certificate) > 2048 || !certificateARN.MatchString(certificate) ||
		!strings.HasPrefix(certificate, "arn:aws:acm:us-east-1:") {
		return invalidParameter("CustomDomainConfig requires an ACM certificate ARN in us-east-1.")
	}

	if !oneOf(config.SecurityPolicy, "TLS_V1", "TLS_V1_2_2021", "TLS_V1_3_2025") {
		return invalidParameter("Invalid custom-domain SecurityPolicy.")
	}

	return nil
}

func (s *Service) CreateUserPoolDomain(
	ctx context.Context, in *api.CreateUserPoolDomainRequest,
) (*api.CreateUserPoolDomainResponse, error) {
	if err := checkDomain(in.Domain, in.CustomDomainConfig, in.ManagedLoginVersion); err != nil {
		return nil, err
	}

	var d *domain
	err := s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		if _, exists := tx.Get(domainKey(scope, p.ID)); exists {
			return invalidParameter("A domain is already configured for this pool.")
		}

		d = &domain{Scope: scope, Description: api.DomainDescriptionType{
			UserPoolId: new(p.ID), AWSAccountId: new(scope.AccountID), Domain: in.Domain,
			ManagedLoginVersion: common.OrElse(in.ManagedLoginVersion, new(int64(1))),
			CustomDomainConfig:  common.Clone(in.CustomDomainConfig), Status: new("ACTIVE"), Version: new("1"),
		}}
		if _, exists := tx.Get(domainIndexKey(d.host())); exists {
			return invalidParameter("Domain already exists.")
		}

		if in.CustomDomainConfig != nil {
			p.CustomDomain = in.Domain
		} else {
			p.Domain = in.Domain
		}

		sum := sha256.Sum256([]byte(scope.AccountID + "/" + p.ID + "/" + *in.Domain))
		d.Description.CloudFrontDistribution = new(fmt.Sprintf("d%x.cloudfront.net", sum[:8]))
		if err := storage.Write(tx, domainIndexKey(d.host()), d); err != nil {
			return err
		}

		return storage.Write(tx, domainKey(scope, p.ID), d)
	})
	if err != nil {
		return nil, err
	}

	out := &api.CreateUserPoolDomainResponse{ManagedLoginVersion: d.Description.ManagedLoginVersion}
	if in.CustomDomainConfig != nil {
		out.CloudFrontDomain = d.Description.CloudFrontDistribution
	}

	return out, nil
}

func (s *Service) DescribeUserPoolDomain(
	ctx context.Context, in *api.DescribeUserPoolDomainRequest,
) (*api.DescribeUserPoolDomainResponse, error) {
	scope, operationErr := request.RegionalScope(ctx)
	if operationErr != nil {
		return nil, operationErr
	}

	out := &api.DescribeUserPoolDomainResponse{DomainDescription: &api.DomainDescriptionType{}}
	operationErr = s.store.View(ctx, func(tx storage.ReadTx) error {
		host := common.Value(in.Domain)
		if !strings.Contains(host, ".") {
			host += ".auth." + scope.Region + ".amazoncognito.com"
		}

		d, ok, err := storage.Read[domain](tx, domainIndexKey(host))
		if err != nil {
			return err
		}

		if ok && d.Scope == scope {
			out.DomainDescription = &d.Description
		}

		return nil
	})
	return out, operationErr
}

func (s *Service) UpdateUserPoolDomain(
	ctx context.Context, in *api.UpdateUserPoolDomainRequest,
) (*api.UpdateUserPoolDomainResponse, error) {
	var d *domain
	err := s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		var ok bool
		var err error
		d, ok, err = storage.Read[domain](tx, domainKey(scope, p.ID))
		if err != nil {
			return err
		}

		if !ok || common.Value(d.Description.Domain) != common.Value(in.Domain) {
			return notFound("Domain does not exist.")
		}

		if in.ManagedLoginVersion != nil {
			d.Description.ManagedLoginVersion = in.ManagedLoginVersion
		}

		if in.CustomDomainConfig != nil {
			if d.Description.CustomDomainConfig == nil {
				return invalidParameter("Cannot change a prefix to a custom domain.")
			}

			d.Description.CustomDomainConfig = common.Clone(in.CustomDomainConfig)
		}

		if err := checkDomain(
			in.Domain,
			d.Description.CustomDomainConfig,
			d.Description.ManagedLoginVersion,
		); err != nil {
			return err
		}

		if err := storage.Write(tx, domainIndexKey(d.host()), d); err != nil {
			return err
		}

		return storage.Write(tx, domainKey(scope, p.ID), d)
	})
	if err != nil {
		return nil, err
	}

	out := &api.UpdateUserPoolDomainResponse{ManagedLoginVersion: d.Description.ManagedLoginVersion}
	if d.Description.CustomDomainConfig != nil {
		out.CloudFrontDomain = d.Description.CloudFrontDistribution
	}

	return out, nil
}

func (s *Service) DeleteUserPoolDomain(
	ctx context.Context, in *api.DeleteUserPoolDomainRequest,
) (*api.DeleteUserPoolDomainResponse, error) {
	err := s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		d, ok, err := storage.Read[domain](tx, domainKey(scope, p.ID))
		if err != nil {
			return err
		}

		if !ok || common.Value(d.Description.Domain) != common.Value(in.Domain) {
			return notFound("Domain does not exist.")
		}

		p.Domain, p.CustomDomain = nil, nil

		tx.Delete(domainIndexKey(d.host()))
		tx.Delete(domainKey(scope, p.ID))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteUserPoolDomainResponse{}, nil
}

// HostedPool resolves a configured hosted domain without trusting an account from the request.
func (s *Service) HostedPool(ctx context.Context, host string) (poolID string, found bool, resultErr error) {
	if hostname, _, err := net.SplitHostPort(host); err == nil {
		host = hostname
	}

	var id string
	resultErr = s.store.View(ctx, func(tx storage.ReadTx) error {
		d, ok, err := storage.Read[domain](tx, domainIndexKey(strings.ToLower(host)))
		if err != nil {
			return err
		}

		if ok {
			id = common.Value(d.Description.UserPoolId)
		}

		return nil
	})
	return id, id != "", resultErr
}
