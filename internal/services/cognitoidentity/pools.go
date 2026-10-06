package cognitoidentity

import (
	"context"
	"crypto/rand"
	"maps"
	"regexp"
	"slices"
	"strings"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidentity"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// pool is an identity pool's record.
type pool struct {
	Pool     api.IdentityPool
	Roles    map[string]string
	Mappings map[string]api.RoleMapping
	// Secret signs the pool's OpenID tokens.
	Secret []byte
}

// socialProviders are the login providers SupportedLoginProviders may name,
// whose tokens TinyStack cannot verify.
var socialProviders = []string{
	"graph.facebook.com", "accounts.google.com", "www.amazon.com", "api.twitter.com", "www.digits.com",
	"appleid.apple.com",
}

var (
	namePattern          = regexp.MustCompile(`^[\w\s+=,.@-]{1,128}$`)
	userPoolPattern      = regexp.MustCompile(`^cognito-idp\.[a-z0-9-]+\.amazonaws\.com/[a-z0-9-]+_[A-Za-z0-9]+$`)
	developerNamePattern = regexp.MustCompile(`^[\w.-]{1,128}$`)
)

func loadPool(tx storage.ReadTx, scope request.Scope, id *string) (*pool, error) {
	p, ok, err := storage.Read[pool](tx, poolKey(scope, common.Value(id)))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, notFound("IdentityPool '%s' not found.", common.Value(id)))
	}

	return p, nil
}

// locatePool reads the pool id names from its partition-wide index, for the
// operations called unsigned.
func locatePool(tx storage.ReadTx, id *string) (request.Scope, *pool, error) {
	m := poolIDPattern.FindStringSubmatch(common.Value(id))
	account, ok := tx.Get(indexPrefix + kindPool + "/" + common.Value(id))
	if m == nil || !ok {
		return request.Scope{}, nil, notFound("IdentityPool '%s' not found.", common.Value(id))
	}

	scope := request.Scope{AccountID: string(account), Region: m[1]}
	p, err := loadPool(tx, scope, id)
	return scope, p, err
}

// checkPool validates a pool's settings.
func checkPool(p *api.IdentityPool) error {
	if !namePattern.MatchString(common.Value(p.IdentityPoolName)) {
		return invalid("IdentityPoolName must be 1 to 128 letters, digits, spaces, or +=,.@-_ characters.")
	}

	if p.AllowUnauthenticatedIdentities == nil {
		return invalid("AllowUnauthenticatedIdentities is required.")
	}

	if name := p.DeveloperProviderName; name != nil && !developerNamePattern.MatchString(*name) {
		return invalid("Invalid DeveloperProviderName %s.", *name)
	}

	for k := range p.SupportedLoginProviders {
		if !slices.Contains(socialProviders, k) {
			return invalid("Invalid login provider %s.", k)
		}
	}

	for _, c := range p.CognitoIdentityProviders {
		if !userPoolPattern.MatchString(common.Value(c.ProviderName)) {
			return invalid("Invalid Cognito user pool provider %s.", common.Value(c.ProviderName))
		}
	}

	for _, arn := range p.OpenIdConnectProviderARNs {
		if !strings.HasPrefix(arn, "arn:aws:iam::") || !strings.Contains(arn, ":oidc-provider/") {
			return invalid("Invalid OpenID Connect provider ARN %s.", arn)
		}
	}

	if err := tagging.CheckSet(p.IdentityPoolTags); err != nil {
		return invalid("%s", err.Error())
	}

	return nil
}

func (s *Service) CreateIdentityPool(ctx context.Context, in *api.CreateIdentityPoolInput) (*api.IdentityPool, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	p := &pool{
		Pool: api.IdentityPool{
			IdentityPoolId:                 new(m.Region + ":" + uuid.NewV4().String()),
			IdentityPoolName:               in.IdentityPoolName,
			AllowUnauthenticatedIdentities: in.AllowUnauthenticatedIdentities,
			AllowClassicFlow:               new(common.Value(in.AllowClassicFlow)),
			SupportedLoginProviders:        in.SupportedLoginProviders,
			DeveloperProviderName:          in.DeveloperProviderName,
			OpenIdConnectProviderARNs:      in.OpenIdConnectProviderARNs,
			CognitoIdentityProviders:       in.CognitoIdentityProviders,
			IdentityPoolTags:               common.CloneTags(in.IdentityPoolTags),
		},
		Roles: map[string]string{}, Secret: make([]byte, 32),
	}
	if checkErr := checkPool(&p.Pool); checkErr != nil {
		return nil, checkErr
	}

	_, _ = rand.Read(p.Secret)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Put(indexPrefix+kindPool+"/"+*p.Pool.IdentityPoolId, []byte(m.AccountID))
		return storage.Write(tx, poolKey(m.Scope, *p.Pool.IdentityPoolId), p)
	})
	if err != nil {
		return nil, err
	}

	return &p.Pool, nil
}

func (s *Service) DescribeIdentityPool(
	ctx context.Context,
	in *api.DescribeIdentityPoolInput,
) (*api.IdentityPool, error) {
	var p *pool
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		p, err = loadPool(tx, scope, in.IdentityPoolId)
		return err
	})
	if err != nil {
		return nil, err
	}

	return &p.Pool, nil
}

func (s *Service) UpdateIdentityPool(ctx context.Context, in *api.IdentityPool) (*api.IdentityPool, error) {
	if err := checkPool(in); err != nil {
		return nil, err
	}

	var p *pool
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		var err error
		if p, err = loadPool(tx, scope, in.IdentityPoolId); err != nil {
			return err
		}

		if name := p.Pool.DeveloperProviderName; name != nil && common.Value(in.DeveloperProviderName) != *name {
			return invalid("The developer provider name cannot be changed.")
		}

		updated := *in
		updated.AllowClassicFlow = new(common.Value(in.AllowClassicFlow))
		if updated.IdentityPoolTags == nil {
			updated.IdentityPoolTags = p.Pool.IdentityPoolTags
		}

		p.Pool = updated
		return storage.Write(tx, poolKey(scope, *in.IdentityPoolId), p)
	})
	if err != nil {
		return nil, err
	}

	return &p.Pool, nil
}

func (s *Service) DeleteIdentityPool(
	ctx context.Context,
	in *api.DeleteIdentityPoolInput,
) (*api.DeleteIdentityPoolOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := loadPool(tx, scope, in.IdentityPoolId); err != nil {
			return err
		}

		id := *in.IdentityPoolId
		identities, err := storage.Records[identity](tx, identityKey(scope, id, ""))
		if err != nil {
			return err
		}

		for _, i := range identities {
			removeIdentity(tx, scope, i)
		}

		tx.Delete(poolKey(scope, id))
		tx.Delete(indexPrefix + kindPool + "/" + id)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteIdentityPoolOutput{}, nil
}

func (s *Service) ListIdentityPools(
	ctx context.Context,
	in *api.ListIdentityPoolsInput,
) (*api.ListIdentityPoolsResponse, error) {
	var all []*pool
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		all, err = storage.Records[pool](tx, scope.Prefix(service, kindPool))
		return err
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := list(all, func(p *pool) string { return *p.Pool.IdentityPoolId }, in.NextToken,
		in.MaxResults)
	if err != nil {
		return nil, err
	}

	out := &api.ListIdentityPoolsResponse{IdentityPools: []api.IdentityPoolShortDescription{}, NextToken: next}
	for _, p := range selected {
		out.IdentityPools = append(out.IdentityPools, api.IdentityPoolShortDescription{
			IdentityPoolId: p.Pool.IdentityPoolId, IdentityPoolName: p.Pool.IdentityPoolName,
		})
	}

	return out, nil
}

// view runs fn in a read transaction in the caller's account and region.
func (s *Service) view(ctx context.Context, fn func(storage.ReadTx, request.Scope) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error { return fn(tx, scope) })
}

// update runs fn in a write transaction in the caller's account and region.
func (s *Service) update(ctx context.Context, fn func(storage.WriteTx, request.Scope) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error { return fn(tx, scope) })
}

// poolOfARN reads the pool an ARN names, the only resource Cognito identity
// tags.
func poolOfARN(tx storage.ReadTx, scope request.Scope, arn *string) (*pool, error) {
	id, ok := strings.CutPrefix(common.Value(arn), poolARN(scope, ""))
	if !ok {
		return nil, invalid("Invalid resource ARN %s.", common.Value(arn))
	}

	return loadPool(tx, scope, &id)
}

func (s *Service) retag(ctx context.Context, arn *string, fn func(map[string]string) map[string]string) error {
	return s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		p, err := poolOfARN(tx, scope, arn)
		if err != nil {
			return err
		}

		p.Pool.IdentityPoolTags = fn(common.CloneTags(p.Pool.IdentityPoolTags))
		if checkErr := tagging.CheckSet(p.Pool.IdentityPoolTags); checkErr != nil {
			return invalid("%s", checkErr.Error())
		}

		return storage.Write(tx, poolKey(scope, *p.Pool.IdentityPoolId), p)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceResponse, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags map[string]string) map[string]string {
		if tags == nil {
			tags = map[string]string{}
		}

		maps.Copy(tags, in.Tags)
		return tags
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceResponse, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags map[string]string) map[string]string {
		return tagging.Remove(tags, in.TagKeys)
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceInput,
) (*api.ListTagsForResourceResponse, error) {
	out := &api.ListTagsForResourceResponse{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		p, err := poolOfARN(tx, scope, in.ResourceArn)
		if err == nil {
			out.Tags = common.CloneTags(p.Pool.IdentityPoolTags)
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	if out.Tags == nil {
		out.Tags = map[string]string{}
	}

	return out, nil
}
