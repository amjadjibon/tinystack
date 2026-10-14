package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/codeconnections"
	"github.com/aws/aws-sdk-go-v2/service/codeconnections/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/smithy-go"
)

func codeConnectionsClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func wantCodeConnectionsError(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestCodeConnectionsSDKLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := codeConnectionsClient(s, "test", "us-east-1")

	created, err := c.CreateConnection(ctx, &sdk.CreateConnectionInput{
		ConnectionName: aws.String("github-main"), ProviderType: types.ProviderTypeGithub,
		Tags: []types.Tag{{Key: aws.String("team"), Value: aws.String("core")}},
	})
	if err != nil || len(created.Tags) != 1 {
		t.Fatal(created, err)
	}

	got, err := c.GetConnection(ctx, &sdk.GetConnectionInput{ConnectionArn: created.ConnectionArn})
	if err != nil || got.Connection.ConnectionStatus != types.ConnectionStatusPending ||
		got.Connection.ProviderType != types.ProviderTypeGithub ||
		aws.ToString(got.Connection.ConnectionName) != "github-main" ||
		aws.ToString(got.Connection.OwnerAccountId) == "" {
		t.Fatal(got, err)
	}

	// A host for GitHub Enterprise Server, then a connection through it.
	host, err := c.CreateHost(ctx, &sdk.CreateHostInput{
		Name: aws.String("ghes"), ProviderType: types.ProviderTypeGithubEnterpriseServer,
		ProviderEndpoint: aws.String("https://ghes.example.com"),
		VpcConfiguration: &types.VpcConfiguration{
			VpcId:            aws.String("vpc-0123456789abcdef0"),
			SubnetIds:        []string{"subnet-1"},
			SecurityGroupIds: []string{"sg-1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	hostGet, err := c.GetHost(ctx, &sdk.GetHostInput{HostArn: host.HostArn})
	if err != nil || aws.ToString(hostGet.Status) != "PENDING" || aws.ToString(hostGet.Name) != "ghes" ||
		hostGet.VpcConfiguration == nil {
		t.Fatal(hostGet, err)
	}

	_, err = c.UpdateHost(ctx, &sdk.UpdateHostInput{
		HostArn: host.HostArn, ProviderEndpoint: aws.String("https://git.example.com"),
	})
	if err != nil {
		t.Fatal(err)
	}

	hosted, err := c.CreateConnection(ctx, &sdk.CreateConnectionInput{
		ConnectionName: aws.String("ghes-conn"), HostArn: host.HostArn,
	})
	if err != nil {
		t.Fatal(err)
	}

	hostedGet, err := c.GetConnection(ctx, &sdk.GetConnectionInput{ConnectionArn: hosted.ConnectionArn})
	if err != nil || hostedGet.Connection.ProviderType != types.ProviderTypeGithubEnterpriseServer ||
		aws.ToString(hostedGet.Connection.HostArn) != aws.ToString(host.HostArn) {
		t.Fatal(hostedGet, err)
	}

	_, err = c.DeleteHost(ctx, &sdk.DeleteHostInput{HostArn: host.HostArn})
	wantCodeConnectionsError(t, err, "ResourceUnavailableException")

	// Listing, filters and pagination.
	all, err := c.ListConnections(ctx, &sdk.ListConnectionsInput{MaxResults: 1})
	if err != nil || len(all.Connections) != 1 || all.NextToken == nil {
		t.Fatal(all, err)
	}

	byHost, err := c.ListConnections(ctx, &sdk.ListConnectionsInput{HostArnFilter: host.HostArn})
	if err != nil || len(byHost.Connections) != 1 {
		t.Fatal(byHost, err)
	}

	byProvider, err := c.ListConnections(ctx, &sdk.ListConnectionsInput{ProviderTypeFilter: types.ProviderTypeGithub})
	if err != nil || len(byProvider.Connections) != 1 {
		t.Fatal(byProvider, err)
	}

	hosts, err := c.ListHosts(ctx, &sdk.ListHostsInput{})
	if err != nil || len(hosts.Hosts) != 1 {
		t.Fatal(hosts, err)
	}

	// Tags, including through the Resource Groups Tagging API.
	_, err = c.TagResource(ctx, &sdk.TagResourceInput{
		ResourceArn: created.ConnectionArn, Tags: []types.Tag{{Key: aws.String("env"), Value: aws.String("dev")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	tagging := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := tagging.TagResources(ctx, &rgta.TagResourcesInput{
		ResourceARNList: []string{aws.ToString(created.ConnectionArn), aws.ToString(host.HostArn)},
		Tags:            map[string]string{"rgta": "works"},
	})
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatal(tagged, err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: created.ConnectionArn})
	if err != nil || len(tags.Tags) != 3 {
		t.Fatal(tags, err)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: created.ConnectionArn, TagKeys: []string{"env"}})
	if err != nil {
		t.Fatal(err)
	}

	// Errors.
	_, err = c.CreateConnection(ctx, &sdk.CreateConnectionInput{ConnectionName: aws.String("no-provider")})
	wantCodeConnectionsError(t, err, "ValidationException")
	_, err = c.CreateConnection(ctx, &sdk.CreateConnectionInput{
		ConnectionName: aws.String("bad-host"),
		HostArn:        aws.String("arn:aws:codeconnections:us-east-1:000000000000:host/missing"),
	})
	wantCodeConnectionsError(t, err, "ResourceNotFoundException")

	// Another account sees none of it.
	other := codeConnectionsClient(s, "team-b", "us-east-1")
	empty, err := other.ListConnections(ctx, &sdk.ListConnectionsInput{})
	if err != nil || len(empty.Connections) != 0 {
		t.Fatal(empty, err)
	}

	_, err = other.GetConnection(ctx, &sdk.GetConnectionInput{ConnectionArn: created.ConnectionArn})
	wantCodeConnectionsError(t, err, "ResourceNotFoundException")

	// Tear down.
	for _, arn := range []*string{created.ConnectionArn, hosted.ConnectionArn} {
		if _, err = c.DeleteConnection(ctx, &sdk.DeleteConnectionInput{ConnectionArn: arn}); err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.DeleteConnection(ctx, &sdk.DeleteConnectionInput{ConnectionArn: created.ConnectionArn})
	wantCodeConnectionsError(t, err, "ResourceNotFoundException")
	if _, err = c.DeleteHost(ctx, &sdk.DeleteHostInput{HostArn: host.HostArn}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetHost(ctx, &sdk.GetHostInput{HostArn: host.HostArn})
	wantCodeConnectionsError(t, err, "ResourceNotFoundException")
}
