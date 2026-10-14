package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/codeartifact"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/smithy-go"
)

func codeArtifactClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func wantCodeArtifactError(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

const artifactPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*",` +
	`"Action":["codeartifact:ReadFromRepository"],"Resource":"*"}]}`

func TestCodeArtifactSDKLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := codeArtifactClient(s, "test", "us-east-1")

	dom, err := c.CreateDomain(ctx, &sdk.CreateDomainInput{
		Domain: aws.String("acme"), Tags: []types.Tag{{Key: aws.String("team"), Value: aws.String("core")}},
	})
	if err != nil || aws.ToString(dom.Domain.EncryptionKey) == "" || dom.Domain.Status != types.DomainStatusActive {
		t.Fatal(dom, err)
	}

	_, err = c.CreateDomain(ctx, &sdk.CreateDomainInput{Domain: aws.String("acme")})
	wantCodeArtifactError(t, err, "ConflictException")
	_, err = c.CreateDomain(ctx, &sdk.CreateDomainInput{Domain: aws.String("Bad_Name")})
	wantCodeArtifactError(t, err, "ValidationException")

	// A repository with an external connection, then one that uses it as an upstream.
	upstream, err := c.CreateRepository(ctx, &sdk.CreateRepositoryInput{
		Domain: aws.String("acme"), Repository: aws.String("npm-store"), Description: aws.String("proxy"),
	})
	if err != nil {
		t.Fatal(err)
	}

	assoc, err := c.AssociateExternalConnection(ctx, &sdk.AssociateExternalConnectionInput{
		Domain: aws.String("acme"), Repository: aws.String("npm-store"), ExternalConnection: aws.String("public:npmjs"),
	})
	if err != nil || len(assoc.Repository.ExternalConnections) != 1 ||
		assoc.Repository.ExternalConnections[0].PackageFormat != types.PackageFormatNpm {
		t.Fatal(assoc, err)
	}

	_, err = c.AssociateExternalConnection(ctx, &sdk.AssociateExternalConnectionInput{
		Domain: aws.String("acme"), Repository: aws.String("npm-store"), ExternalConnection: aws.String("public:pypi"),
	})
	wantCodeArtifactError(t, err, "ConflictException")
	_, err = c.AssociateExternalConnection(ctx, &sdk.AssociateExternalConnectionInput{
		Domain: aws.String("acme"), Repository: aws.String("npm-store"), ExternalConnection: aws.String("public:nope"),
	})
	wantCodeArtifactError(t, err, "ValidationException")

	repo, err := c.CreateRepository(ctx, &sdk.CreateRepositoryInput{
		Domain: aws.String("acme"), Repository: aws.String("main"),
		Upstreams: []types.UpstreamRepository{{RepositoryName: aws.String("npm-store")}},
	})
	if err != nil || len(repo.Repository.Upstreams) != 1 {
		t.Fatal(repo, err)
	}

	_, err = c.CreateRepository(ctx, &sdk.CreateRepositoryInput{
		Domain: aws.String("acme"), Repository: aws.String("bad"),
		Upstreams: []types.UpstreamRepository{{RepositoryName: aws.String("ghost")}},
	})
	wantCodeArtifactError(t, err, "ResourceNotFoundException")

	// An upstream cycle is refused.
	_, err = c.UpdateRepository(ctx, &sdk.UpdateRepositoryInput{
		Domain: aws.String("acme"), Repository: aws.String("npm-store"),
		Upstreams: []types.UpstreamRepository{{RepositoryName: aws.String("main")}},
	})
	wantCodeArtifactError(t, err, "ValidationException")

	updated, err := c.UpdateRepository(ctx, &sdk.UpdateRepositoryInput{
		Domain: aws.String("acme"), Repository: aws.String("main"), Description: aws.String("primary"),
	})
	if err != nil || aws.ToString(updated.Repository.Description) != "primary" ||
		len(updated.Repository.Upstreams) != 1 {
		t.Fatal(updated, err)
	}

	_, err = c.DeleteRepository(
		ctx,
		&sdk.DeleteRepositoryInput{Domain: aws.String("acme"), Repository: aws.String("npm-store")},
	)
	wantCodeArtifactError(t, err, "ConflictException")

	// Listing.
	all, err := c.ListRepositories(ctx, &sdk.ListRepositoriesInput{MaxResults: aws.Int32(1)})
	if err != nil || len(all.Repositories) != 1 || all.NextToken == nil {
		t.Fatal(all, err)
	}

	inDomain, err := c.ListRepositoriesInDomain(ctx, &sdk.ListRepositoriesInDomainInput{
		Domain: aws.String("acme"), RepositoryPrefix: aws.String("ma"),
	})
	if err != nil || len(inDomain.Repositories) != 1 {
		t.Fatal(inDomain, err)
	}

	domains, err := c.ListDomains(ctx, &sdk.ListDomainsInput{})
	if err != nil || len(domains.Domains) != 1 {
		t.Fatal(domains, err)
	}

	desc, err := c.DescribeDomain(ctx, &sdk.DescribeDomainInput{Domain: aws.String("acme")})
	if err != nil || desc.Domain.RepositoryCount != 2 {
		t.Fatal(desc, err)
	}

	// Permissions policies.
	_, err = c.GetDomainPermissionsPolicy(ctx, &sdk.GetDomainPermissionsPolicyInput{Domain: aws.String("acme")})
	wantCodeArtifactError(t, err, "ResourceNotFoundException")
	_, err = c.PutDomainPermissionsPolicy(ctx, &sdk.PutDomainPermissionsPolicyInput{
		Domain: aws.String("acme"), PolicyDocument: aws.String("not json"),
	})
	wantCodeArtifactError(t, err, "MalformedPolicyException")

	put, err := c.PutDomainPermissionsPolicy(ctx, &sdk.PutDomainPermissionsPolicyInput{
		Domain: aws.String("acme"), PolicyDocument: aws.String(artifactPolicy),
	})
	if err != nil || aws.ToString(put.Policy.Revision) == "" {
		t.Fatal(put, err)
	}

	_, err = c.PutDomainPermissionsPolicy(ctx, &sdk.PutDomainPermissionsPolicyInput{
		Domain: aws.String("acme"), PolicyDocument: aws.String(artifactPolicy), PolicyRevision: aws.String("stale"),
	})
	wantCodeArtifactError(t, err, "ConflictException")

	got, err := c.GetDomainPermissionsPolicy(ctx, &sdk.GetDomainPermissionsPolicyInput{Domain: aws.String("acme")})
	if err != nil || aws.ToString(got.Policy.Document) != artifactPolicy {
		t.Fatal(got, err)
	}

	repoPut, err := c.PutRepositoryPermissionsPolicy(ctx, &sdk.PutRepositoryPermissionsPolicyInput{
		Domain: aws.String("acme"), Repository: aws.String("main"), PolicyDocument: aws.String(artifactPolicy),
	})
	if err != nil {
		t.Fatal(err)
	}

	repoDel, err := c.DeleteRepositoryPermissionsPolicy(ctx, &sdk.DeleteRepositoryPermissionsPolicyInput{
		Domain: aws.String("acme"), Repository: aws.String("main"), PolicyRevision: repoPut.Policy.Revision,
	})
	if err != nil || aws.ToString(repoDel.Policy.Revision) != aws.ToString(repoPut.Policy.Revision) {
		t.Fatal(repoDel, err)
	}

	_, err = c.GetRepositoryPermissionsPolicy(ctx, &sdk.GetRepositoryPermissionsPolicyInput{
		Domain: aws.String("acme"), Repository: aws.String("main"),
	})
	wantCodeArtifactError(t, err, "ResourceNotFoundException")

	// Token and endpoint.
	token, err := c.GetAuthorizationToken(ctx, &sdk.GetAuthorizationTokenInput{Domain: aws.String("acme")})
	if err != nil || aws.ToString(token.AuthorizationToken) == "" || token.Expiration == nil {
		t.Fatal(token, err)
	}

	_, err = c.GetAuthorizationToken(
		ctx,
		&sdk.GetAuthorizationTokenInput{Domain: aws.String("acme"), DurationSeconds: aws.Int64(5)},
	)
	wantCodeArtifactError(t, err, "ValidationException")

	endpoint, err := c.GetRepositoryEndpoint(ctx, &sdk.GetRepositoryEndpointInput{
		Domain: aws.String("acme"), Repository: aws.String("main"), Format: types.PackageFormatNpm,
	})
	if err != nil || !strings.HasPrefix(aws.ToString(endpoint.RepositoryEndpoint), s.URL) {
		t.Fatal(endpoint, err)
	}

	// Tags, including through the Resource Groups Tagging API.
	tagging := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := tagging.TagResources(ctx, &rgta.TagResourcesInput{
		ResourceARNList: []string{aws.ToString(dom.Domain.Arn), aws.ToString(repo.Repository.Arn)},
		Tags:            map[string]string{"rgta": "works"},
	})
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatal(tagged, err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: dom.Domain.Arn})
	if err != nil || len(tags.Tags) != 2 {
		t.Fatal(tags, err)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: dom.Domain.Arn, TagKeys: []string{"team"}})
	if err != nil {
		t.Fatal(err)
	}

	// Another account sees none of it.
	other := codeArtifactClient(s, "team-b", "us-east-1")
	_, err = other.DescribeDomain(ctx, &sdk.DescribeDomainInput{Domain: aws.String("acme")})
	wantCodeArtifactError(t, err, "ResourceNotFoundException")

	// Tear down in dependency order.
	_, err = c.DisassociateExternalConnection(ctx, &sdk.DisassociateExternalConnectionInput{
		Domain: aws.String("acme"), Repository: aws.String("npm-store"), ExternalConnection: aws.String("public:npmjs"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteDomain(ctx, &sdk.DeleteDomainInput{Domain: aws.String("acme")})
	wantCodeArtifactError(t, err, "ConflictException")
	for _, name := range []string{"main", "npm-store"} {
		if _, err = c.DeleteRepository(ctx, &sdk.DeleteRepositoryInput{
			Domain: aws.String("acme"), Repository: aws.String(name),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err = c.DeleteDomain(ctx, &sdk.DeleteDomainInput{Domain: aws.String("acme")}); err != nil {
		t.Fatal(err)
	}

	_ = upstream
}
