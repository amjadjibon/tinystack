package apigateway

import (
	"context"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Custom domain names are "…/apigateway/domainname/<name>", with their base
// path mappings, and the partition-wide "aws/apigateway/domain/<name>" finds
// a domain's account and Region (domain names are unique) when a request
// addresses it. A request to a domain is a request to the REST API stage its
// longest matching base path maps to. Certificates are stored, never
// checked, and no TLS is served.

const (
	kindDomain   = "domainname"
	noBasePath   = "(none)"
	endpointEdge = "EDGE"
	// regionalZone and cloudFrontZone are the hosted zones Route 53 alias
	// records to regional and edge-optimized domains name.
	regionalZone   = "Z1UJRXOUMOOFQ8"
	cloudFrontZone = "Z2FDTNDATAQYW2"
)

var (
	domainPattern   = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	basePathPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]+(/[A-Za-z0-9._~-]+)*$`)
)

// domain is a custom domain name's record.
type domain struct {
	Domain   api.DomainName
	Mappings []api.BasePathMapping
}

func domainKey(scope request.Scope, name string) string { return prefix(scope, kindDomain) + name }

func domainIndexKey(name string) string { return "aws/" + service + "/domain/" + name }

func domainNotFound() error { return notFound("Invalid domain name identifier specified") }

func loadDomain(tx storage.ReadTx, scope request.Scope, name string) (*domain, error) {
	d, found, err := storage.Read[domain](tx, domainKey(scope, strings.ToLower(name)))
	if err == nil && !found {
		err = domainNotFound()
	}

	return d, err
}

func (s *Service) CreateDomainName(ctx context.Context, in *api.CreateDomainNameRequest) (*api.DomainName, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name := strings.ToLower(common.Value(in.DomainName))
	if !domainPattern.MatchString(name) || len(name) > 253 {
		return nil, badRequest("Invalid domain name: %s", common.Value(in.DomainName))
	}

	types := []string{endpointEdge}
	if in.EndpointConfiguration != nil && len(in.EndpointConfiguration.Types) > 0 {
		types = in.EndpointConfiguration.Types
	}

	edge := slices.Contains(types, endpointEdge)
	switch {
	case len(types) != 1 || (!edge && types[0] != "REGIONAL"):
		return nil, badRequest("A domain name's endpoint type must be EDGE or REGIONAL.")
	case edge && in.CertificateArn == nil:
		return nil, badRequest("An edge-optimized domain name needs a certificateArn.")
	case !edge && in.RegionalCertificateArn == nil:
		return nil, badRequest("A regional domain name needs a regionalCertificateArn.")
	}

	if tagErr := checkTags(in.Tags); tagErr != nil {
		return nil, tagErr
	}

	label := "d-" + common.RandomID(10)
	d := &domain{Domain: api.DomainName{
		DomainName:    &name,
		DomainNameArn: new("arn:aws:apigateway:" + m.Region + "::/domainnames/" + name),
		EndpointConfiguration: &api.EndpointConfiguration{
			Types: types, IpAddressType: common.OrElse(endpointIPType(in.EndpointConfiguration), new("ipv4")),
		},
		DomainNameStatus: new("AVAILABLE"), SecurityPolicy: common.OrElse(in.SecurityPolicy, new("TLS_1_2")),
		Tags: common.CloneTags(in.Tags),
	}, Mappings: []api.BasePathMapping{}}
	if edge {
		d.Domain.CertificateArn, d.Domain.CertificateName = in.CertificateArn, in.CertificateName
		d.Domain.CertificateUploadDate = common.Seconds(s.now())
		d.Domain.DistributionDomainName = new(label + ".cloudfront.net")
		d.Domain.DistributionHostedZoneId = new(cloudFrontZone)
	} else {
		d.Domain.RegionalCertificateArn, d.Domain.RegionalCertificateName = in.RegionalCertificateArn,
			in.RegionalCertificateName
		d.Domain.RegionalDomainName = new(label + ".execute-api." + m.Region + ".amazonaws.com")
		d.Domain.RegionalHostedZoneId = new(regionalZone)
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, exists := tx.Get(domainIndexKey(name)); exists {
			return conflict("The domain name you provided already exists.")
		}

		if writeErr := storage.Write(
			tx,
			domainIndexKey(name),
			location{Account: m.AccountID, Region: m.Region},
		); writeErr != nil {
			return writeErr
		}

		return storage.Write(tx, domainKey(m.Scope, name), d)
	})
	if err != nil {
		return nil, err
	}

	return &d.Domain, nil
}

func endpointIPType(c *api.EndpointConfiguration) *string {
	if c == nil {
		return nil
	}

	return c.IpAddressType
}

func (s *Service) GetDomainName(ctx context.Context, in *api.GetDomainNameRequest) (*api.DomainName, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.DomainName
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		d, loadErr := loadDomain(tx, m.Scope, common.Value(in.DomainName))
		if loadErr == nil {
			out = &d.Domain
		}

		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetDomainNames(ctx context.Context, in *api.GetDomainNamesRequest) (*api.DomainNames, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var domains []*domain
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		domains, readErr = storage.Records[domain](tx, prefix(m.Scope, kindDomain))
		return readErr
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(domains, func(d *domain) string { return *d.Domain.DomainName }, in.Position, in.Limit)
	if err != nil {
		return nil, err
	}

	out := &api.DomainNames{Items: []api.DomainName{}, Position: next}
	for _, d := range selected {
		out.Items = append(out.Items, d.Domain)
	}

	return out, nil
}

func (s *Service) DeleteDomainName(
	ctx context.Context,
	in *api.DeleteDomainNameRequest,
) (*api.DeleteDomainNameOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		d, loadErr := loadDomain(tx, m.Scope, common.Value(in.DomainName))
		if loadErr != nil {
			return loadErr
		}

		tx.Delete(domainIndexKey(*d.Domain.DomainName))
		tx.Delete(domainKey(m.Scope, *d.Domain.DomainName))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteDomainNameOutput{}, nil
}

// basePath normalizes a base path, "(none)" for the empty one.
func basePath(p *string) string {
	if v := strings.Trim(common.Value(p), "/"); v != "" {
		return v
	}

	return noBasePath
}

// CreateBasePathMapping maps a base path of a domain (none by default) to a
// REST API, and to one of its stages or, without a stage, to the stage the
// path names next.
func (s *Service) CreateBasePathMapping(
	ctx context.Context,
	in *api.CreateBasePathMappingRequest,
) (*api.BasePathMapping, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	path := basePath(in.BasePath)
	if path != noBasePath && !basePathPattern.MatchString(path) {
		return nil, badRequest("Invalid base path: %s", path)
	}

	mapping := &api.BasePathMapping{BasePath: &path, RestApiId: in.RestApiId, Stage: in.Stage}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		d, loadErr := loadDomain(tx, m.Scope, common.Value(in.DomainName))
		if loadErr != nil {
			return loadErr
		}

		if _, apiErr := loadAPI(tx, m.Scope, in.RestApiId); apiErr != nil {
			return badRequest("Invalid REST API identifier specified")
		}

		if in.Stage != nil {
			if _, found := tx.Get(owned(m.Scope, kindStage, *in.RestApiId, *in.Stage)); !found {
				return badRequest("Invalid stage identifier specified")
			}
		}

		if slices.ContainsFunc(d.Mappings, func(b api.BasePathMapping) bool { return *b.BasePath == path }) {
			return conflict("Base path already exists for this domain name")
		}

		d.Mappings = append(d.Mappings, *mapping)
		slices.SortFunc(
			d.Mappings,
			func(a, b api.BasePathMapping) int { return strings.Compare(*a.BasePath, *b.BasePath) },
		)
		return storage.Write(tx, domainKey(m.Scope, *d.Domain.DomainName), d)
	})
	if err != nil {
		return nil, err
	}

	return mapping, nil
}

func (s *Service) GetBasePathMapping(
	ctx context.Context,
	in *api.GetBasePathMappingRequest,
) (*api.BasePathMapping, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	path := basePath(in.BasePath)
	var out *api.BasePathMapping
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		d, loadErr := loadDomain(tx, m.Scope, common.Value(in.DomainName))
		if loadErr != nil {
			return loadErr
		}

		i := slices.IndexFunc(d.Mappings, func(b api.BasePathMapping) bool { return *b.BasePath == path })
		if i < 0 {
			return notFound("Invalid base path mapping identifier specified")
		}

		out = &d.Mappings[i]
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetBasePathMappings(
	ctx context.Context,
	in *api.GetBasePathMappingsRequest,
) (*api.BasePathMappings, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var mappings []api.BasePathMapping
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		d, loadErr := loadDomain(tx, m.Scope, common.Value(in.DomainName))
		if loadErr == nil {
			mappings = d.Mappings
		}

		return loadErr
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(
		mappings,
		func(b api.BasePathMapping) string { return *b.BasePath },
		in.Position,
		in.Limit,
	)
	if err != nil {
		return nil, err
	}

	return &api.BasePathMappings{Items: append([]api.BasePathMapping{}, selected...), Position: next}, nil
}

func (s *Service) DeleteBasePathMapping(
	ctx context.Context,
	in *api.DeleteBasePathMappingRequest,
) (*api.DeleteBasePathMappingOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	path := basePath(in.BasePath)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		d, loadErr := loadDomain(tx, m.Scope, common.Value(in.DomainName))
		if loadErr != nil {
			return loadErr
		}

		kept := slices.DeleteFunc(
			slices.Clone(d.Mappings),
			func(b api.BasePathMapping) bool { return *b.BasePath == path },
		)
		if len(kept) == len(d.Mappings) {
			return notFound("Invalid base path mapping identifier specified")
		}

		d.Mappings = kept
		return storage.Write(tx, domainKey(m.Scope, *d.Domain.DomainName), d)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteBasePathMappingOutput{}, nil
}

// Domains serves requests to custom domain names: id is the domain name.
type Domains struct{ s *Service }

// Domains returns the execute.Handler that serves custom domain names.
func (s *Service) Domains() *Domains { return &Domains{s: s} }

// Known reports whether a host (without its port) is a custom domain name.
func (h *Domains) Known(ctx context.Context, host string) bool {
	found := false
	_ = h.s.store.View(ctx, func(tx storage.ReadTx) error {
		_, found = tx.Get(domainIndexKey(strings.ToLower(host)))
		return nil
	})

	return found
}

// Execute runs a request to a domain as a request to the REST API stage its
// longest matching base path maps to.
func (h *Domains) Execute(ctx context.Context, name string, r *execute.Request) (*execute.Response, bool, error) {
	name = strings.ToLower(name)
	var d *domain
	err := h.s.store.View(ctx, func(tx storage.ReadTx) error {
		loc, found, err := storage.Read[location](tx, domainIndexKey(name))
		if err != nil || !found {
			return err
		}

		d, err = loadDomain(tx, request.Scope{AccountID: loc.Account, Region: loc.Region}, name)
		return err
	})
	if err != nil || d == nil {
		return nil, false, err
	}

	path := strings.TrimPrefix(r.Path, "/")
	var best *api.BasePathMapping
	rest := ""
	for i := range d.Mappings {
		b := &d.Mappings[i]
		switch after, ok := strings.CutPrefix(path, *b.BasePath); {
		case *b.BasePath == noBasePath && best == nil:
			best, rest = b, path
		case ok && (after == "" || after[0] == '/') && (best == nil || len(*b.BasePath) > len(*best.BasePath) ||
			*best.BasePath == noBasePath):
			best, rest = b, strings.TrimPrefix(after, "/")
		}
	}

	if best == nil {
		return execute.Message(http.StatusForbidden, "Forbidden"), true, nil
	}

	mapped := *r
	mapped.Path = "/" + rest
	if best.Stage != nil {
		mapped.Path = "/" + *best.Stage + "/" + rest
	}

	return h.s.Execute(ctx, *best.RestApiId, &mapped)
}
