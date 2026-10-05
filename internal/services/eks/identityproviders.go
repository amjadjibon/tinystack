package eks

import (
	"context"
	"net/url"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/eks"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindIdentityProvider = "identity-provider"
	typeOIDC             = "oidc"
	maxRequiredClaims    = 10
)

// identityProvider is an OIDC identity provider config's record: its
// description and its associate token.
type identityProvider struct {
	Config api.OidcIdentityProviderConfig
	Token  string
}

func identityProviderKey(scope request.Scope, clusterName, name string) string {
	return owned(scope, kindIdentityProvider, clusterName, name)
}

// configParam is the update parameter that names an identity provider config.
func configParam(name string) api.UpdateParam {
	return jsonParam("IdentityProviderConfig", []map[string]string{{"type": typeOIDC, "name": name}})
}

func checkOIDC(o *api.OidcIdentityProviderConfigRequest) error {
	if o == nil {
		return invalidParameter("oidc is required.")
	}

	if err := checkName(o.IdentityProviderConfigName, "identityProviderConfigName", 100); err != nil {
		return err
	}

	issuer, err := url.Parse(common.Value(o.IssuerUrl))
	switch {
	case err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.RawQuery != "" || issuer.Fragment != "":
		return invalidParameter("issuerUrl must be an https URL without a query or fragment.")
	case common.Value(o.ClientId) == "":
		return invalidParameter("clientId is required.")
	case len(o.RequiredClaims) > maxRequiredClaims:
		return invalidParameter("requiredClaims can have at most 10 claims.")
	}

	for k, v := range o.RequiredClaims {
		if k == "" || len(k) > 63 || v == "" || len(v) > 253 {
			return invalidParameter("Required claim keys are 1 to 63 characters and values 1 to 253.")
		}
	}

	return nil
}

// AssociateIdentityProviderConfig associates an OIDC identity provider with a
// cluster, which has at most one; the config is ACTIVE at once.
func (s *Service) AssociateIdentityProviderConfig(
	ctx context.Context,
	in *api.AssociateIdentityProviderConfigRequest,
) (*api.AssociateIdentityProviderConfigResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if checkErr := checkOIDC(in.Oidc); checkErr != nil {
		return nil, checkErr
	}

	if checkErr := checkTags(in.Tags); checkErr != nil {
		return nil, checkErr
	}

	out := &api.AssociateIdentityProviderConfigResponse{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, loadErr := loadCluster(tx, scope, in.ClusterName)
		if loadErr != nil {
			return loadErr
		}

		existing, readErr := storage.Records[identityProvider](
			tx,
			scope.Prefix(service, kindIdentityProvider)+c.Name+"/",
		)
		if readErr != nil {
			return readErr
		}

		if len(existing) > 0 {
			return inUse("Identity provider config %s is already associated with cluster %s.",
				*existing[0].Config.IdentityProviderConfigName, c.Name)
		}

		o := in.Oidc
		name := *o.IdentityProviderConfigName
		p := &identityProvider{Token: common.Value(in.ClientRequestToken), Config: api.OidcIdentityProviderConfig{
			IdentityProviderConfigName: o.IdentityProviderConfigName, ClusterName: new(c.Name),
			IdentityProviderConfigArn: new(arn(scope, "identityproviderconfig/"+c.Name+"/"+typeOIDC+"/"+name+"/"+
				uuid.NewV4().String())),
			IssuerUrl: o.IssuerUrl, ClientId: o.ClientId, UsernameClaim: o.UsernameClaim,
			UsernamePrefix: o.UsernamePrefix, GroupsClaim: o.GroupsClaim, GroupsPrefix: o.GroupsPrefix,
			RequiredClaims: o.RequiredClaims, Tags: common.CloneTags(in.Tags), Status: new("ACTIVE"),
		}}

		update, recordErr := s.record(tx, scope, c.Name, "", "", "AssociateIdentityProviderConfig",
			[]api.UpdateParam{configParam(name)})
		if recordErr != nil {
			return recordErr
		}

		out.Update, out.Tags = update, p.Config.Tags
		return storage.Write(tx, identityProviderKey(scope, c.Name, name), p)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// findIdentityProvider reads the config a request names, which must be an
// OIDC config.
func findIdentityProvider(tx storage.ReadTx, scope request.Scope, clusterName *string,
	ref *api.IdentityProviderConfig,
) (*cluster, *identityProvider, error) {
	if ref == nil || common.Value(ref.Type) != typeOIDC {
		return nil, nil, invalidParameter("identityProviderConfig must name an oidc config.")
	}

	c, err := loadCluster(tx, scope, clusterName)
	if err != nil {
		return nil, nil, err
	}

	p, ok, err := storage.Read[identityProvider](tx, identityProviderKey(scope, c.Name, common.Value(ref.Name)))
	if err != nil || !ok {
		return nil, nil, common.ErrorsOr(
			err,
			notFound("Identity provider config %s not found.", common.Value(ref.Name)),
		)
	}

	return c, p, nil
}

func (s *Service) DescribeIdentityProviderConfig(
	ctx context.Context,
	in *api.DescribeIdentityProviderConfigRequest,
) (*api.DescribeIdentityProviderConfigResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var p *identityProvider
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var findErr error
		_, p, findErr = findIdentityProvider(tx, scope, in.ClusterName, in.IdentityProviderConfig)
		return findErr
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeIdentityProviderConfigResponse{
		IdentityProviderConfig: &api.IdentityProviderConfigResponse{Oidc: &p.Config},
	}, nil
}

func (s *Service) ListIdentityProviderConfigs(
	ctx context.Context,
	in *api.ListIdentityProviderConfigsRequest,
) (*api.ListIdentityProviderConfigsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListIdentityProviderConfigsResponse{IdentityProviderConfigs: []api.IdentityProviderConfig{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		c, loadErr := loadCluster(tx, scope, in.ClusterName)
		if loadErr != nil {
			return loadErr
		}

		all, readErr := storage.Records[identityProvider](tx, scope.Prefix(service, kindIdentityProvider)+c.Name+"/")
		if readErr != nil {
			return readErr
		}

		var refs []api.IdentityProviderConfig
		for _, p := range all {
			refs = append(refs, api.IdentityProviderConfig{
				Type: new(typeOIDC), Name: p.Config.IdentityProviderConfigName,
			})
		}

		refs, out.NextToken, readErr = page(refs, func(r api.IdentityProviderConfig) string { return *r.Name },
			in.NextToken, in.MaxResults)
		out.IdentityProviderConfigs = append(out.IdentityProviderConfigs, refs...)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// DisassociateIdentityProviderConfig removes a cluster's identity provider
// config at once.
func (s *Service) DisassociateIdentityProviderConfig(
	ctx context.Context,
	in *api.DisassociateIdentityProviderConfigRequest,
) (*api.DisassociateIdentityProviderConfigResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.DisassociateIdentityProviderConfigResponse{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, p, findErr := findIdentityProvider(tx, scope, in.ClusterName, in.IdentityProviderConfig)
		if findErr != nil {
			return findErr
		}

		name := *p.Config.IdentityProviderConfigName
		update, recordErr := s.record(tx, scope, c.Name, "", "", "DisassociateIdentityProviderConfig",
			[]api.UpdateParam{configParam(name)})
		if recordErr != nil {
			return recordErr
		}

		out.Update = update
		tx.Delete(identityProviderKey(scope, c.Name, name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
