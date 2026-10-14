package codeartifact

import (
	"context"
	"slices"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/codeartifact"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	// A token lasts 12 hours unless the caller asks for 15 minutes to 12 hours; 0 asks for the role's remaining
	// session, which TinyStack treats as the 12 hour default.
	defaultTokenSeconds = 12 * 60 * 60
	minTokenSeconds     = 900
)

// formats are the PackageFormat values of the model.
var formats = []string{"npm", "pypi", formatMaven, "nuget", "generic", "ruby", "swift", "cargo"}

// GetAuthorizationToken returns an opaque token. TinyStack serves no package endpoint, so nothing accepts it.
func (s *Service) GetAuthorizationToken(
	ctx context.Context,
	in *api.GetAuthorizationTokenRequest,
) (*api.GetAuthorizationTokenResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	domain, err := checkDomain(scope, in.Domain, in.DomainOwner)
	if err != nil {
		return nil, err
	}

	seconds := int64(defaultTokenSeconds)
	if in.DurationSeconds != nil && *in.DurationSeconds != 0 {
		if *in.DurationSeconds < minTokenSeconds || *in.DurationSeconds > defaultTokenSeconds {
			return nil, invalid("durationSeconds must be 0 or between %d and %d.", minTokenSeconds, defaultTokenSeconds)
		}

		seconds = *in.DurationSeconds
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if _, found := tx.Get(domainKey(scope, domain)); !found {
			return domainNotFound(domain)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	expires := time.Now().Add(time.Duration(seconds) * time.Second)

	return &api.GetAuthorizationTokenResult{
		AuthorizationToken: new("tinystack-codeartifact-" + common.RandomID(96, common.AlphabetAlphanumeric)),
		Expiration:         common.Seconds(expires),
	}, nil
}

// GetRepositoryEndpoint returns a URL under TinyStack's address in the shape of a CodeArtifact endpoint. Nothing
// serves it: package managers cannot publish to or install from it.
func (s *Service) GetRepositoryEndpoint(
	ctx context.Context,
	in *api.GetRepositoryEndpointRequest,
) (*api.GetRepositoryEndpointResult, error) {
	meta, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	domain, name, err := target(meta.Scope, in.Domain, in.DomainOwner, in.Repository)
	if err != nil {
		return nil, err
	}

	format := common.Value(in.Format)
	if !slices.Contains(formats, format) {
		return nil, invalid("format %q is not a package format.", format)
	}

	if t := common.Value(in.EndpointType); t != "" && t != "ipv4" && t != "dualstack" {
		return nil, invalid("endpointType must be ipv4 or dualstack.")
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, e := readRepository(tx, meta.Scope, domain, name)
		return e
	})
	if err != nil {
		return nil, err
	}

	base := meta.Endpoint
	if base == "" {
		base = "https://" + domain + "-" + meta.AccountID + ".d.codeartifact." + meta.Region + ".amazonaws.com"
	}

	return &api.GetRepositoryEndpointResult{
		RepositoryEndpoint: new(
			base + "/codeartifact/" + domain + "/" + meta.AccountID + "/" + format + "/" + name + "/",
		),
	}, nil
}
