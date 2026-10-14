package cognitoidp

import (
	"context"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var (
	providerName       = regexp.MustCompile(`^[^_\p{Z}][\p{L}\p{M}\p{S}\p{N}\p{P}][^_\p{Z}]+$`)
	providerIdentifier = regexp.MustCompile(`^[\w\s+=.@-]+$`)
)

func providerKey(scope request.Scope, pool, name string) string {
	return scope.Key(service, "provider", pool+"/"+name)
}

func checkProvider(p *pool, idp *api.IdentityProviderType) error {
	name := common.Value(idp.ProviderName)
	if utf8.RuneCountInString(name) > 32 || !providerName.MatchString(name) || name == providerCognito {
		return invalidParameter("Invalid ProviderName.")
	}

	if idp.ProviderType == nil || !oneOf(idp.ProviderType, providerOIDC, "SAML", "Google", "Facebook",
		"LoginWithAmazon", "SignInWithApple") {
		return invalidParameter("Invalid ProviderType.")
	}

	if idp.ProviderDetails == nil {
		return invalidParameter("ProviderDetails is required.")
	}

	for key, value := range idp.ProviderDetails {
		if utf8.RuneCountInString(key) > 131072 || utf8.RuneCountInString(value) > 131072 {
			return invalidParameter("ProviderDetails strings must be at most 131072 characters.")
		}
	}

	if len(idp.IdpIdentifiers) > 50 {
		return invalidParameter("At most 50 IdpIdentifiers are allowed.")
	}

	for _, id := range idp.IdpIdentifiers {
		if len(id) > 40 || !providerIdentifier.MatchString(id) {
			return invalidParameter("Invalid IdpIdentifier.")
		}
	}

	for attr, claim := range idp.AttributeMapping {
		a := p.attribute(attr)
		if a == nil || attr == attrSub || attr == attrIdentities || claim == "" || len(attr) > 32 ||
			utf8.RuneCountInString(claim) > 131072 {
			return invalidParameter("Invalid attribute mapping for %s.", attr)
		}

		if !common.Value(a.Mutable) {
			return invalidParameter("Mapped attribute %s must be mutable.", attr)
		}
	}

	if *idp.ProviderType != providerOIDC {
		return nil
	}

	details := idp.ProviderDetails
	allowed := []string{
		"attributes_request_method", "attributes_url", "authorize_scopes", "authorize_url",
		claimClientID, "client_secret", providerJWKS, "oidc_issuer", "token_url", "attributes_url_add_attributes",
	}

	for k := range details {
		if !slices.Contains(allowed, k) {
			return invalidParameter("Unsupported OIDC ProviderDetails key %s.", k)
		}
	}

	if details[claimClientID] == "" || details["client_secret"] == "" || details["oidc_issuer"] == "" ||
		!slices.Contains(strings.Fields(details["authorize_scopes"]), "openid") {
		return invalidParameter("OIDC requires client_id, client_secret, oidc_issuer and scopes including openid.")
	}

	for _, key := range []string{"oidc_issuer", "authorize_url", "token_url", providerJWKS, "attributes_url"} {
		if details[key] == "" {
			continue
		}

		if !validOIDCEndpoint(details[key]) {
			return invalidParameter("Invalid OIDC %s URL.", key)
		}
	}

	if method := details["attributes_request_method"]; method != "" && method != "GET" && method != "POST" {
		return invalidParameter("attributes_request_method must be GET or POST.")
	}

	if v := details["attributes_url_add_attributes"]; v != "" && v != "false" {
		return invalidParameter("OIDC attributes_url_add_attributes must be false.")
	}

	return nil
}

func loadProvider(tx storage.ReadTx, scope request.Scope, p *pool, name *string) (*api.IdentityProviderType, error) {
	idp, ok, err := storage.Read[api.IdentityProviderType](tx, providerKey(scope, p.ID, common.Value(name)))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, notFound("Identity provider does not exist.")
	}

	return idp, nil
}

func checkClientProviders(tx storage.ReadTx, scope request.Scope, p *pool, settings *clientSettings) error {
	for _, name := range settings.SupportedIdentityProviders {
		if name == providerCognito {
			continue
		}

		if _, err := loadProvider(tx, scope, p, &name); err != nil {
			return invalidParameter("The provider %s does not exist for User Pool %s", name, p.ID)
		}
	}

	return nil
}

func (s *Service) CreateIdentityProvider(
	ctx context.Context, in *api.CreateIdentityProviderRequest,
) (*api.CreateIdentityProviderResponse, error) {
	idp, operationErr := convert[api.IdentityProviderType](in)
	if operationErr != nil {
		return nil, operationErr
	}

	operationErr = s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		if err := checkProvider(p, &idp); err != nil {
			return err
		}

		if _, exists := tx.Get(providerKey(scope, p.ID, *in.ProviderName)); exists {
			return awserror.New("DuplicateProviderException", "Identity provider already exists.")
		}

		if err := uniqueIdentifiers(tx, scope, p, &idp); err != nil {
			return err
		}

		idp.CreationDate, idp.LastModifiedDate = common.SecondsMilli(s.now()), common.SecondsMilli(s.now())
		return storage.Write(tx, providerKey(scope, p.ID, *in.ProviderName), idp)
	})
	if operationErr != nil {
		return nil, operationErr
	}

	return &api.CreateIdentityProviderResponse{IdentityProvider: &idp}, nil
}

func uniqueIdentifiers(tx storage.ReadTx, scope request.Scope, p *pool, candidate *api.IdentityProviderType) error {
	records, err := storage.Records[api.IdentityProviderType](tx, scope.Prefix(service, "provider")+p.ID+"/")
	if err != nil {
		return err
	}

	for _, idp := range records {
		if common.Value(idp.ProviderName) == common.Value(candidate.ProviderName) {
			continue
		}

		for _, id := range candidate.IdpIdentifiers {
			if slices.Contains(idp.IdpIdentifiers, id) {
				return invalidParameter("IdpIdentifier already exists.")
			}
		}
	}

	return nil
}

func (s *Service) DescribeIdentityProvider(
	ctx context.Context, in *api.DescribeIdentityProviderRequest,
) (*api.DescribeIdentityProviderResponse, error) {
	var idp *api.IdentityProviderType
	err := s.view(ctx, in.UserPoolId, func(tx storage.ReadTx, scope request.Scope, p *pool) error {
		var err error
		idp, err = loadProvider(tx, scope, p, in.ProviderName)
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeIdentityProviderResponse{IdentityProvider: idp}, nil
}

func (s *Service) UpdateIdentityProvider(
	ctx context.Context, in *api.UpdateIdentityProviderRequest,
) (*api.UpdateIdentityProviderResponse, error) {
	var idp *api.IdentityProviderType
	err := s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		var err error
		idp, err = loadProvider(tx, scope, p, in.ProviderName)
		if err != nil {
			return err
		}

		if in.ProviderDetails != nil {
			for k, v := range in.ProviderDetails {
				idp.ProviderDetails[k] = v
			}
		}

		if in.AttributeMapping != nil {
			idp.AttributeMapping = cloneMap(in.AttributeMapping)
		}

		if in.IdpIdentifiers != nil {
			idp.IdpIdentifiers = slices.Clone(in.IdpIdentifiers)
		}

		if err := checkProvider(p, idp); err != nil {
			return err
		}

		if err := uniqueIdentifiers(tx, scope, p, idp); err != nil {
			return err
		}

		idp.LastModifiedDate = common.SecondsMilli(s.now())
		return storage.Write(tx, providerKey(scope, p.ID, *in.ProviderName), idp)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateIdentityProviderResponse{IdentityProvider: idp}, nil
}

func (s *Service) DeleteIdentityProvider(
	ctx context.Context, in *api.DeleteIdentityProviderRequest,
) (*api.DeleteIdentityProviderOutput, error) {
	err := s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		if _, err := loadProvider(tx, scope, p, in.ProviderName); err != nil {
			return err
		}

		clients, err := storage.Records[client](tx, scope.Prefix(service, "client")+p.ID+"/")
		if err != nil {
			return err
		}

		for _, c := range clients {
			c.Settings.SupportedIdentityProviders = slices.DeleteFunc(c.Settings.SupportedIdentityProviders,
				func(name string) bool { return name == *in.ProviderName })
			if err := storage.Write(tx, clientKey(scope, p.ID, c.ID), c); err != nil {
				return err
			}
		}

		tx.Delete(providerKey(scope, p.ID, *in.ProviderName))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteIdentityProviderOutput{}, nil
}

func (s *Service) ListIdentityProviders(
	ctx context.Context, in *api.ListIdentityProvidersRequest,
) (*api.ListIdentityProvidersResponse, error) {
	n, operationErr := limit(in.MaxResults, "MaxResults")
	if operationErr != nil {
		return nil, operationErr
	}

	var records []*api.IdentityProviderType
	operationErr = s.view(ctx, in.UserPoolId, func(tx storage.ReadTx, scope request.Scope, p *pool) error {
		var err error
		records, err = storage.Records[api.IdentityProviderType](tx, scope.Prefix(service, "provider")+p.ID+"/")
		return err
	})
	if operationErr != nil {
		return nil, operationErr
	}

	selected, next, operationErr := common.Page(records, func(p *api.IdentityProviderType) string {
		return common.Value(p.ProviderName)
	}, in.NextToken, n, errPageToken)
	if operationErr != nil {
		return nil, operationErr
	}

	out := &api.ListIdentityProvidersResponse{Providers: []api.ProviderDescription{}, NextToken: next}
	for _, idp := range selected {
		out.Providers = append(out.Providers, api.ProviderDescription{
			ProviderName: idp.ProviderName,
			ProviderType: idp.ProviderType, CreationDate: idp.CreationDate, LastModifiedDate: idp.LastModifiedDate,
		})
	}

	return out, nil
}

func (s *Service) GetIdentityProviderByIdentifier(
	ctx context.Context, in *api.GetIdentityProviderByIdentifierRequest,
) (*api.GetIdentityProviderByIdentifierResponse, error) {
	var found *api.IdentityProviderType
	err := s.view(ctx, in.UserPoolId, func(tx storage.ReadTx, scope request.Scope, p *pool) error {
		records, err := storage.Records[api.IdentityProviderType](tx, scope.Prefix(service, "provider")+p.ID+"/")
		if err != nil {
			return err
		}

		for _, idp := range records {
			if slices.Contains(idp.IdpIdentifiers, common.Value(in.IdpIdentifier)) {
				found = idp
				return nil
			}
		}

		return notFound("Identity provider identifier does not exist.")
	})
	if err != nil {
		return nil, err
	}

	return &api.GetIdentityProviderByIdentifierResponse{IdentityProvider: found}, nil
}

// ProtectProviderCredentials redacts ProviderDetails, whose string-map model can contain client secrets
// and private keys without any Smithy sensitive trait.
//
//nolint:gocritic // Startup decoration preserves the generated value descriptor contract.
func ProtectProviderCredentials(descriptor catalog.Service) catalog.Service {
	for _, name := range []string{
		"CreateIdentityProvider", "UpdateIdentityProvider", "DescribeIdentityProvider",
		"GetIdentityProviderByIdentifier",
	} {
		operation := descriptor.Operations[name]
		operation.Sensitive = true
		descriptor.Operations[name] = operation
	}

	return descriptor
}

// validOIDCEndpoint permits local HTTP providers, while preventing active-content redirects and URL credentials.
func validOIDCEndpoint(raw string) bool {
	endpoint, err := url.Parse(raw)
	return err == nil && endpoint.Host != "" && endpoint.User == nil && endpoint.Fragment == "" &&
		(endpoint.Scheme == "https" || endpoint.Scheme == "http")
}
