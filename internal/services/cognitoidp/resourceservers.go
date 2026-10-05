package cognitoidp

import (
	"context"
	"net/url"
	"regexp"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resource servers define the custom scopes ("<identifier>/<scope>") that app
// clients may be allowed and the client credentials grant issues. Each is
// "…/cognitoidp/resource-server/<pool ID>/<path-escaped identifier>".

const (
	kindResourceServer = "resource-server"
	maxResourceServers = 25
	maxScopes          = 100
)

var (
	identifierPattern = regexp.MustCompile(`^[\x21\x23-\x5B\x5D-\x7E]{1,256}$`)
	scopeNamePattern  = regexp.MustCompile(`^[\x21\x23-\x2E\x30-\x5B\x5D-\x7E]{1,256}$`)
)

func resourceServerKey(scope request.Scope, pool, identifier string) string {
	return prefix(scope, kindResourceServer) + pool + "/" + url.PathEscape(identifier)
}

func resourceServerNotFound(identifier string) error {
	return awserror.New("ResourceNotFoundException", "Resource server "+identifier+" does not exist.")
}

func checkResourceServer(identifier, name *string, scopes []api.ResourceServerScopeType) error {
	switch {
	case identifier == nil || !identifierPattern.MatchString(*identifier):
		return invalidParameter("Identifier must be 1 to 256 printable characters, without quotes or backslashes.")
	case name == nil || *name == "" || len(*name) > 256:
		return invalidParameter("Name must be 1 to 256 characters.")
	case len(scopes) > maxScopes:
		return invalidParameter("A resource server can have at most %d scopes.", maxScopes)
	}

	seen := map[string]bool{}
	for _, sc := range scopes {
		n := value(sc.ScopeName)
		switch {
		case !scopeNamePattern.MatchString(n):
			return invalidParameter("Invalid scope name %s.", n)
		case value(sc.ScopeDescription) == "" || len(*sc.ScopeDescription) > 256:
			return invalidParameter("ScopeDescription must be 1 to 256 characters.")
		case seen[n]:
			return invalidParameter("Scope %s is repeated.", n)
		}

		seen[n] = true
	}

	return nil
}

// customScopes returns the full names of a pool's resource server scopes.
func customScopes(tx storage.ReadTx, scope request.Scope, pool string) ([]string, error) {
	var out []string
	for _, k := range tx.Keys(prefix(scope, kindResourceServer) + pool + "/") {
		rs, _, err := read[api.ResourceServerType](tx, k)
		if err != nil {
			return nil, err
		}

		for _, sc := range rs.Scopes {
			out = append(out, *rs.Identifier+"/"+*sc.ScopeName)
		}
	}

	return out, nil
}

func (s *Service) CreateResourceServer(
	ctx context.Context,
	in *api.CreateResourceServerRequest,
) (*api.CreateResourceServerResponse, error) {
	if err := checkResourceServer(in.Identifier, in.Name, in.Scopes); err != nil {
		return nil, err
	}

	var rs *api.ResourceServerType
	err := s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		key := resourceServerKey(scope, p.ID, *in.Identifier)
		if _, exists := tx.Get(key); exists {
			return invalidParameter("%s already exists in pool %s.", *in.Identifier, p.ID)
		}

		if len(tx.Keys(prefix(scope, kindResourceServer)+p.ID+"/")) >= maxResourceServers {
			return awserror.New("LimitExceededException", "The user pool has reached its limit of resource servers.")
		}

		rs = &api.ResourceServerType{UserPoolId: new(p.ID), Identifier: in.Identifier, Name: in.Name, Scopes: in.Scopes}
		if rs.Scopes == nil {
			rs.Scopes = []api.ResourceServerScopeType{}
		}

		return write(tx, key, rs)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateResourceServerResponse{ResourceServer: rs}, nil
}

func (s *Service) DescribeResourceServer(
	ctx context.Context,
	in *api.DescribeResourceServerRequest,
) (*api.DescribeResourceServerResponse, error) {
	var rs *api.ResourceServerType
	err := s.view(ctx, in.UserPoolId, func(tx storage.ReadTx, scope request.Scope, p *pool) error {
		var found bool
		var readErr error
		rs, found, readErr = read[api.ResourceServerType](tx, resourceServerKey(scope, p.ID, value(in.Identifier)))
		if readErr == nil && !found {
			return resourceServerNotFound(value(in.Identifier))
		}

		return readErr
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeResourceServerResponse{ResourceServer: rs}, nil
}

// UpdateResourceServer replaces a resource server's name and scopes. App
// clients keep the scopes it no longer has.
func (s *Service) UpdateResourceServer(
	ctx context.Context,
	in *api.UpdateResourceServerRequest,
) (*api.UpdateResourceServerResponse, error) {
	if err := checkResourceServer(in.Identifier, in.Name, in.Scopes); err != nil {
		return nil, err
	}

	var rs *api.ResourceServerType
	err := s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		key := resourceServerKey(scope, p.ID, *in.Identifier)
		if _, exists := tx.Get(key); !exists {
			return resourceServerNotFound(*in.Identifier)
		}

		rs = &api.ResourceServerType{UserPoolId: new(p.ID), Identifier: in.Identifier, Name: in.Name, Scopes: in.Scopes}
		if rs.Scopes == nil {
			rs.Scopes = []api.ResourceServerScopeType{}
		}

		return write(tx, key, rs)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateResourceServerResponse{ResourceServer: rs}, nil
}

func (s *Service) DeleteResourceServer(
	ctx context.Context,
	in *api.DeleteResourceServerRequest,
) (*api.DeleteResourceServerOutput, error) {
	err := s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		key := resourceServerKey(scope, p.ID, value(in.Identifier))
		if _, exists := tx.Get(key); !exists {
			return resourceServerNotFound(value(in.Identifier))
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteResourceServerOutput{}, nil
}

// ListResourceServers lists a pool's resource servers in identifier order.
func (s *Service) ListResourceServers(
	ctx context.Context,
	in *api.ListResourceServersRequest,
) (*api.ListResourceServersResponse, error) {
	n, err := limit(in.MaxResults, "MaxResults")
	if err != nil {
		return nil, err
	}

	var servers []api.ResourceServerType
	err = s.view(ctx, in.UserPoolId, func(tx storage.ReadTx, scope request.Scope, p *pool) error {
		for _, k := range tx.Keys(prefix(scope, kindResourceServer) + p.ID + "/") {
			rs, _, readErr := read[api.ResourceServerType](tx, k)
			if readErr != nil {
				return readErr
			}

			servers = append(servers, *rs)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(
		servers,
		func(rs api.ResourceServerType) string { return *rs.Identifier },
		in.NextToken,
		n,
	)
	if err != nil {
		return nil, err
	}

	return &api.ListResourceServersResponse{
		ResourceServers: append([]api.ResourceServerType{}, selected...),
		NextToken:       next,
	}, nil
}
