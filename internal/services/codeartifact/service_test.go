package codeartifact_test

import (
	"context"
	"errors"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codeartifact"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/codeartifact"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func testContext() context.Context {
	return request.WithMetadata(context.Background(), request.Metadata{
		Scope: request.Scope{AccountID: "123456789012", Region: "us-east-1"}, Endpoint: "http://127.0.0.1:4566",
	})
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *awserror.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestOwnerAndNameRules(t *testing.T) {
	s := codeartifact.New(memory.New(nil, nil))
	ctx := testContext()

	for _, name := range []string{"a", "A-b", "1abc", "abc-", "has_underscore"} {
		_, err := s.CreateDomain(ctx, &api.CreateDomainRequest{Domain: new(name)})
		wantCode(t, err, "ValidationException")
	}

	if _, err := s.CreateDomain(ctx, &api.CreateDomainRequest{Domain: new("acme")}); err != nil {
		t.Fatal(err)
	}

	// Another owner is rejected, not ignored.
	_, err := s.DescribeDomain(ctx, &api.DescribeDomainRequest{Domain: new("acme"), DomainOwner: new("999999999999")})
	wantCode(t, err, "ValidationException")
	_, err = s.GetRepositoryEndpoint(ctx, &api.GetRepositoryEndpointRequest{
		Domain: new("acme"), Repository: new("nope"), Format: new("npm"),
	})
	wantCode(t, err, "ResourceNotFoundException")
}

func TestUpstreamRules(t *testing.T) {
	s := codeartifact.New(memory.New(nil, nil))
	ctx := testContext()
	if _, err := s.CreateDomain(ctx, &api.CreateDomainRequest{Domain: new("acme")}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"one", "two"} {
		if _, err := s.CreateRepository(
			ctx,
			&api.CreateRepositoryRequest{Domain: new("acme"), Repository: new(name)},
		); err != nil {
			t.Fatal(err)
		}
	}

	update := func(repo string, upstreams ...string) error {
		var list []api.UpstreamRepository
		for _, u := range upstreams {
			list = append(list, api.UpstreamRepository{RepositoryName: new(u)})
		}

		_, err := s.UpdateRepository(ctx, &api.UpdateRepositoryRequest{
			Domain: new("acme"), Repository: new(repo), Upstreams: list,
		})
		return err
	}

	wantCode(t, update("one", "one"), "ValidationException")
	wantCode(t, update("one", "two", "two"), "ValidationException")

	if err := update("one", "two"); err != nil {
		t.Fatal(err)
	}

	wantCode(t, update("two", "one"), "ValidationException")
}
