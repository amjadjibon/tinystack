package codeconnections_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codeconnections"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/codeconnections"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func testContext(account, region string) context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: account, Region: region}},
	)
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *awserror.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestConnectionValidation(t *testing.T) {
	s := codeconnections.New(memory.New(nil, nil))
	ctx := testContext("123456789012", "us-east-1")

	for name, in := range map[string]*api.CreateConnectionInput{
		"no name":          {ProviderType: new("GitHub")},
		"long name":        {ConnectionName: new(strings.Repeat("a", 33)), ProviderType: new("GitHub")},
		"no provider":      {ConnectionName: new("a")},
		"unknown provider": {ConnectionName: new("a"), ProviderType: new("Svn")},
		"reserved tag":     {ConnectionName: new("a"), ProviderType: new("GitHub"), Tags: []api.Tag{{Key: new("aws:x")}}},
		"malformed host":   {ConnectionName: new("a"), HostArn: new("nope")},
		"connection as host": {
			ConnectionName: new("a"), HostArn: new("arn:aws:codeconnections:us-east-1:123456789012:connection/x"),
		},
	} {
		if _, err := s.CreateConnection(ctx, in); err == nil {
			t.Fatalf("%s: want an error", name)
		}
	}

	out, err := s.CreateConnection(
		ctx, &api.CreateConnectionInput{ConnectionName: new("a"), ProviderType: new("Bitbucket")},
	)
	if err != nil {
		t.Fatal(err)
	}

	// A connection's ARN in another Region is not found, not invalid.
	elsewhere := testContext("123456789012", "eu-west-1")
	_, err = s.GetConnection(elsewhere, &api.GetConnectionInput{ConnectionArn: out.ConnectionArn})
	wantCode(t, err, "ResourceNotFoundException")
	_, err = s.GetHost(ctx, &api.GetHostInput{HostArn: out.ConnectionArn})
	wantCode(t, err, "ValidationException")
}

func TestHostRules(t *testing.T) {
	s := codeconnections.New(memory.New(nil, nil))
	ctx := testContext("123456789012", "us-east-1")

	_, err := s.CreateHost(ctx, &api.CreateHostInput{
		Name: new("h"), ProviderType: new("GitHub"), ProviderEndpoint: new("https://x"),
	})
	wantCode(t, err, "ValidationException")

	host, err := s.CreateHost(ctx, &api.CreateHostInput{
		Name: new("h"), ProviderType: new("GitHubEnterpriseServer"), ProviderEndpoint: new("https://x"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.UpdateHost(ctx, &api.UpdateHostInput{HostArn: host.HostArn})
	wantCode(t, err, "ValidationException")
	_, err = s.CreateConnection(ctx, &api.CreateConnectionInput{
		ConnectionName: new("c"), ProviderType: new("GitLab"), HostArn: host.HostArn,
	})
	wantCode(t, err, "ValidationException")
}
