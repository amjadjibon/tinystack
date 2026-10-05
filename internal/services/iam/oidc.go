package iam

import (
	"context"
	"net/url"
	"regexp"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// kindOIDC records are OpenID Connect providers, keyed by their URL without
// the scheme, as their ARNs name them.
const (
	kindOIDC       = "oidc-provider"
	maxClientIDs   = 100
	maxThumbprints = 5
)

var thumbprint = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// oidcProvider is an OpenID Connect provider that web identity tokens can
// come from.
type oidcProvider struct {
	Tags        map[string]string `json:",omitempty"`
	URL         string
	ClientIDs   []string
	Thumbprints []string `json:",omitempty"`
	CreatedAt   int64
}

// providerURL is a provider URL without its scheme, as IAM names providers.
func providerURL(raw *string) (string, error) {
	if err := required("Url", raw); err != nil {
		return "", err
	}

	u, err := url.Parse(*raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || len(*raw) > 255 {
		return "", validation("The URL %s is not a valid HTTPS URL without a query or fragment", *raw)
	}

	return u.Host + strings.TrimSuffix(u.Path, "/"), nil
}

// providerName is the URL an OpenID Connect provider ARN names in account.
func providerName(account string, arn *string) (string, error) {
	if err := required("OpenIDConnectProviderArn", arn); err != nil {
		return "", err
	}

	name, ok := strings.CutPrefix(*arn, "arn:aws:iam::"+account+":"+kindOIDC+"/")
	if !ok || name == "" {
		return "", invalidInput("Invalid OpenIDConnectProviderArn %s", *arn)
	}

	return name, nil
}

func checkThumbprints(list []string) error {
	if len(list) > maxThumbprints {
		return limitExceeded("A provider can have at most %d thumbprints", maxThumbprints)
	}

	for _, t := range list {
		if !thumbprint.MatchString(t) {
			return validation("Thumbprint %s must be 40 hexadecimal characters", t)
		}
	}

	return nil
}

func checkClientID(id string) error {
	if id == "" || len(id) > 255 {
		return validation("A client ID must be 1 to 255 characters long")
	}

	return nil
}

func (s *Service) CreateOpenIDConnectProvider(
	ctx context.Context,
	in *api.CreateOpenIDConnectProviderRequest,
) (*api.CreateOpenIDConnectProviderResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name, err := providerURL(in.Url)
	if err != nil {
		return nil, err
	}

	if len(in.ClientIDList) > maxClientIDs {
		return nil, limitExceeded("A provider can have at most %d client IDs", maxClientIDs)
	}

	for _, id := range in.ClientIDList {
		if guardErr := checkClientID(id); guardErr != nil {
			return nil, guardErr
		}
	}

	if guardErr := checkThumbprints(in.ThumbprintList); guardErr != nil {
		return nil, guardErr
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if guardErr := tagError(tagging.Check(tags)); guardErr != nil {
		return nil, guardErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := key(m.AccountID, kindOIDC, name)
		if _, exists := tx.Get(k); exists {
			return alreadyExists("Provider with url https://%s already exists.", name)
		}

		ids := slices.Compact(slices.Sorted(slices.Values(in.ClientIDList)))
		return storage.Write(tx, k, &oidcProvider{
			Tags: tags, URL: name, ClientIDs: ids, Thumbprints: in.ThumbprintList, CreatedAt: s.now().UnixMilli(),
		})
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateOpenIDConnectProviderResponse{
		OpenIDConnectProviderArn: new(arn(m.AccountID, kindOIDC+"/"+name)), Tags: tagList(tags),
	}, nil
}

// loadProvider reads the provider an ARN names in account.
func loadProvider(tx storage.ReadTx, account string, providerARN *string) (*oidcProvider, string, error) {
	name, err := providerName(account, providerARN)
	if err != nil {
		return nil, "", err
	}

	k := key(account, kindOIDC, name)
	p, found, err := storage.Read[oidcProvider](tx, k)
	if err == nil && !found {
		err = noSuchEntity("OpenIDConnect Provider not found for arn %s", *providerARN)
	}

	return p, k, err
}

func (s *Service) GetOpenIDConnectProvider(
	ctx context.Context,
	in *api.GetOpenIDConnectProviderRequest,
) (*api.GetOpenIDConnectProviderResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var p *oidcProvider
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		p, _, loadErr = loadProvider(tx, m.AccountID, in.OpenIDConnectProviderArn)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return &api.GetOpenIDConnectProviderResponse{
		Url: new(p.URL), ClientIDList: p.ClientIDs, ThumbprintList: p.Thumbprints, CreateDate: millis(p.CreatedAt),
		Tags: tagList(p.Tags),
	}, nil
}

func (s *Service) ListOpenIDConnectProviders(
	ctx context.Context,
	_ *api.ListOpenIDConnectProvidersRequest,
) (*api.ListOpenIDConnectProvidersResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListOpenIDConnectProvidersResponse{OpenIDConnectProviderList: []api.OpenIDConnectProviderListEntry{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		providers, listErr := list[oidcProvider](tx, m.AccountID, kindOIDC)
		for _, p := range providers {
			out.OpenIDConnectProviderList = append(out.OpenIDConnectProviderList, api.OpenIDConnectProviderListEntry{
				Arn: new(arn(m.AccountID, kindOIDC+"/"+p.URL)),
			})
		}

		return listErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// updateProvider applies change to the provider an ARN names.
func (s *Service) updateProvider(ctx context.Context, providerARN *string, change func(*oidcProvider) error) error {
	m, err := identity(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, k, loadErr := loadProvider(tx, m.AccountID, providerARN)
		if loadErr != nil {
			return loadErr
		}

		if changeErr := change(p); changeErr != nil {
			return changeErr
		}

		return storage.Write(tx, k, p)
	})
}

func (s *Service) DeleteOpenIDConnectProvider(
	ctx context.Context,
	in *api.DeleteOpenIDConnectProviderRequest,
) (*api.DeleteOpenIDConnectProviderOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, k, loadErr := loadProvider(tx, m.AccountID, in.OpenIDConnectProviderArn)
		if loadErr != nil {
			return loadErr
		}

		tx.Delete(k)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteOpenIDConnectProviderOutput{}, nil
}

func (s *Service) AddClientIDToOpenIDConnectProvider(
	ctx context.Context,
	in *api.AddClientIDToOpenIDConnectProviderRequest,
) (*api.AddClientIDToOpenIDConnectProviderOutput, error) {
	id := common.Value(in.ClientID)
	if err := checkClientID(id); err != nil {
		return nil, err
	}

	err := s.updateProvider(ctx, in.OpenIDConnectProviderArn, func(p *oidcProvider) error {
		if slices.Contains(p.ClientIDs, id) {
			return nil
		}

		if len(p.ClientIDs) == maxClientIDs {
			return limitExceeded("A provider can have at most %d client IDs", maxClientIDs)
		}

		p.ClientIDs = slices.Sorted(slices.Values(append(p.ClientIDs, id)))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.AddClientIDToOpenIDConnectProviderOutput{}, nil
}

func (s *Service) RemoveClientIDFromOpenIDConnectProvider(
	ctx context.Context,
	in *api.RemoveClientIDFromOpenIDConnectProviderRequest,
) (*api.RemoveClientIDFromOpenIDConnectProviderOutput, error) {
	id := common.Value(in.ClientID)
	err := s.updateProvider(ctx, in.OpenIDConnectProviderArn, func(p *oidcProvider) error {
		p.ClientIDs = slices.DeleteFunc(p.ClientIDs, func(c string) bool { return c == id })
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.RemoveClientIDFromOpenIDConnectProviderOutput{}, nil
}

func (s *Service) UpdateOpenIDConnectProviderThumbprint(
	ctx context.Context,
	in *api.UpdateOpenIDConnectProviderThumbprintRequest,
) (*api.UpdateOpenIDConnectProviderThumbprintOutput, error) {
	if err := checkThumbprints(in.ThumbprintList); err != nil {
		return nil, err
	}

	err := s.updateProvider(ctx, in.OpenIDConnectProviderArn, func(p *oidcProvider) error {
		p.Thumbprints = in.ThumbprintList
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateOpenIDConnectProviderThumbprintOutput{}, nil
}

// Provider is what STS needs to know to accept a web identity token.
type Provider struct {
	ARN       string
	ClientIDs []string
}

// OIDCProvider returns the account's OpenID Connect provider for a token
// issuer, such as "https://token.actions.githubusercontent.com", or false
// when it has none.
func (s *Service) OIDCProvider(ctx context.Context, account, issuer string) (Provider, bool, error) {
	name, err := providerURL(&issuer)
	if err != nil {
		return Provider{}, false, nil //nolint:nilerr // an issuer that is no HTTPS URL has no provider.
	}

	var out Provider
	found := false
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p, exists, readErr := storage.Read[oidcProvider](tx, key(account, kindOIDC, name))
		if readErr != nil || !exists {
			return readErr
		}

		found = true
		out = Provider{ARN: arn(account, kindOIDC+"/"+p.URL), ClientIDs: p.ClientIDs}
		return nil
	})

	return out, found, err
}
