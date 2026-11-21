package apigatewayv2

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindDomain     = "domainname"
	apiMappingOnly = "API_MAPPING_ONLY"
)

func domainKey(scope request.Scope, name string) string {
	return scope.Prefix(service, kindDomain) + strings.ToLower(name)
}

func loadDomain(tx storage.ReadTx, scope request.Scope, name string) (*api.DomainName, error) {
	domain, found, err := storage.Read[api.DomainName](tx, domainKey(scope, name))
	if err == nil && !found {
		err = notFound("Invalid domain name identifier specified")
	}

	return domain, err
}

func (s *Service) CreateDomainName(
	ctx context.Context, in *api.CreateDomainNameRequest,
) (*api.CreateDomainNameResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name := strings.ToLower(common.Value(in.DomainName))
	if !proxy.ValidDomainName(name) {
		return nil, badRequest("Invalid domain name: %s", common.Value(in.DomainName))
	}

	if in.RoutingMode != nil && *in.RoutingMode != apiMappingOnly {
		return nil, badRequest("Only API_MAPPING_ONLY routing is supported.")
	}

	if tagErr := checkTags(in.Tags); tagErr != nil {
		return nil, tagErr
	}

	config, err := s.domainConfiguration(in.DomainNameConfigurations, m.Region)
	if err != nil {
		return nil, err
	}

	domain := api.DomainName{
		DomainName: new(name), DomainNameArn: new("arn:aws:apigateway:" + m.Region + "::/domainnames/" + name),
		ApiMappingSelectionExpression: new(
			"$request.basepath",
		), DomainNameConfigurations: []api.DomainNameConfiguration{
			config,
		},
		RoutingMode: new(apiMappingOnly), Tags: common.CloneTags(in.Tags),
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, gateway := range []string{"apigateway", service} {
			if _, exists := tx.Get(proxy.DomainIndexKey(gateway, name)); exists {
				return conflict("The domain name you provided already exists.")
			}
		}

		loc := location{Account: m.AccountID, Region: m.Region}
		if writeErr := storage.Write(tx, proxy.DomainIndexKey(service, name), loc); writeErr != nil {
			return writeErr
		}

		return storage.Write(tx, domainKey(m.Scope, name), domain)
	})
	if err != nil {
		return nil, err
	}

	return new(api.CreateDomainNameResponse(domain)), nil
}

func (s *Service) domainConfiguration(
	configs []api.DomainNameConfiguration, region string,
) (api.DomainNameConfiguration, error) {
	var config api.DomainNameConfiguration
	switch len(configs) {
	case 0:
	case 1:
		config = configs[0]
	default:
		return config, badRequest("Only one domain name configuration is supported.")
	}

	if common.Value(common.OrElse(config.EndpointType, new("REGIONAL"))) != "REGIONAL" ||
		common.Value(common.OrElse(config.SecurityPolicy, new("TLS_1_2"))) != "TLS_1_2" {
		return config, badRequest("HTTP API domains support REGIONAL endpoints with the TLS_1_2 policy.")
	}

	if config.ApiGatewayDomainName != nil || config.DomainNameStatus != nil ||
		config.HostedZoneId != nil || config.CertificateUploadDate != nil {
		return config, badRequest("The domain endpoint, status, hosted zone and certificate upload date are read-only.")
	}

	config.EndpointType, config.SecurityPolicy = new("REGIONAL"), new("TLS_1_2")
	config.DomainNameStatus = new("AVAILABLE")
	config.ApiGatewayDomainName = new("d-" + common.RandomID(10) + ".execute-api." + region + ".amazonaws.com")
	if config.CertificateArn != nil {
		config.CertificateUploadDate = new(s.now().UTC())
	}

	return config, nil
}

func (s *Service) GetDomainName(
	ctx context.Context, in *api.GetDomainNameRequest,
) (*api.GetDomainNameResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.DomainName
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		out, loadErr = loadDomain(tx, m.Scope, common.Value(in.DomainName))
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return (*api.GetDomainNameResponse)(out), nil
}

func (s *Service) GetDomainNames(
	ctx context.Context, in *api.GetDomainNamesRequest,
) (*api.GetDomainNamesResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.GetDomainNamesResponse{Items: []api.DomainName{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		domains, readErr := storage.Records[api.DomainName](tx, m.Prefix(service, kindDomain))
		if readErr != nil {
			return readErr
		}

		selected, next, pageErr := page(domains, func(d *api.DomainName) string { return *d.DomainName },
			in.NextToken, in.MaxResults)
		out.NextToken = next
		for _, domain := range selected {
			out.Items = append(out.Items, *domain)
		}

		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteDomainName(
	ctx context.Context, in *api.DeleteDomainNameRequest,
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

		if len(tx.Keys(m.Prefix(service, kindAPIMapping)+*d.DomainName+"/")) > 0 {
			return conflict("Delete the API mappings before deleting the domain name.")
		}

		tx.Delete(domainKey(m.Scope, *d.DomainName))
		tx.Delete(proxy.DomainIndexKey(service, *d.DomainName))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteDomainNameOutput{}, nil
}

// Domains serves the HTTP APIs mapped to a configured Host.
type Domains struct{ s *Service }

func (s *Service) Domains() *Domains { return &Domains{s: s} }

func (d *Domains) Known(ctx context.Context, host string) bool {
	var found bool
	_ = d.s.store.View(ctx, func(tx storage.ReadTx) error {
		_, found = tx.Get(proxy.DomainIndexKey(service, strings.ToLower(host)))
		return nil
	})

	return found
}

func (d *Domains) Execute(ctx context.Context, host string, r *execute.Request) (*execute.Response, bool, error) {
	name, path := strings.ToLower(host), strings.TrimPrefix(r.Path, "/")
	var known bool
	var best *api.ApiMapping
	err := d.s.store.View(ctx, func(tx storage.ReadTx) error {
		loc, found, err := storage.Read[location](tx, proxy.DomainIndexKey(service, name))
		known = found
		if err != nil || !found {
			return err
		}

		scope := request.Scope{AccountID: loc.Account, Region: loc.Region}
		items, err := storage.Records[api.ApiMapping](tx, scope.Prefix(service, kindAPIMapping)+name+"/")
		if err != nil {
			return err
		}

		multilevel := slices.ContainsFunc(items, func(m *api.ApiMapping) bool {
			return strings.Contains(common.Value(m.ApiMappingKey), "/")
		})
		for _, mapping := range items {
			key := common.Value(mapping.ApiMappingKey)
			after, prefixed := strings.CutPrefix(path, key)
			matched := key == "" || prefixed && (multilevel || after == "" || after[0] == '/')
			if matched && (best == nil || len(key) > len(common.Value(best.ApiMappingKey))) {
				best = mapping
			}
		}

		return nil
	})
	if err != nil || !known {
		return nil, known, err
	}

	if best == nil {
		return execute.Message(http.StatusForbidden, "Forbidden"), true, nil
	}

	mapped := *r
	if key := common.Value(best.ApiMappingKey); key != "" {
		mapped.Path = "/" + strings.TrimPrefix(path[len(key):], "/")
	}

	resp, found, err := d.s.execute(ctx, *best.ApiId, &mapped, *best.Stage)
	if err == nil && !found {
		resp = execute.Message(http.StatusNotFound, "Not Found")
	}

	return resp, true, err
}
