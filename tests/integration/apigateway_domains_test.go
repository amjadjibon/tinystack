package integration_test

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	restapi "github.com/aws/aws-sdk-go-v2/service/apigateway"
	resttypes "github.com/aws/aws-sdk-go-v2/service/apigateway/types"
)

// getStatus sends a GET to the server with a Host header, returning the
// status and body.
func getStatus(t *testing.T, url, host string) (status int, body string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	if host != "" {
		req.Host = host
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func TestRESTAPICustomDomains(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := restClient(s)
	api, err := c.CreateRestApi(ctx, &restapi.CreateRestApiInput{Name: aws.String("branded")})
	if err != nil {
		t.Fatal(err)
	}

	pets, err := c.CreateResource(ctx, &restapi.CreateResourceInput{
		RestApiId: api.Id, ParentId: api.RootResourceId, PathPart: aws.String("pets"),
	})
	if err != nil {
		t.Fatal(err)
	}

	mockGET := func(resource *string, body string) {
		if _, err = c.PutMethod(ctx, &restapi.PutMethodInput{
			RestApiId:         api.Id,
			ResourceId:        resource,
			HttpMethod:        aws.String("GET"),
			AuthorizationType: aws.String("NONE"),
		}); err != nil {
			t.Fatal(err)
		}

		if _, err = c.PutIntegration(ctx, &restapi.PutIntegrationInput{
			RestApiId: api.Id, ResourceId: resource, HttpMethod: aws.String("GET"), Type: resttypes.IntegrationTypeMock,
			RequestTemplates: map[string]string{"application/json": `{"statusCode": 200}`},
		}); err != nil {
			t.Fatal(err)
		}

		if _, err = c.PutMethodResponse(ctx, &restapi.PutMethodResponseInput{
			RestApiId: api.Id, ResourceId: resource, HttpMethod: aws.String("GET"), StatusCode: aws.String("200"),
		}); err != nil {
			t.Fatal(err)
		}

		if _, err = c.PutIntegrationResponse(ctx, &restapi.PutIntegrationResponseInput{
			RestApiId: api.Id, ResourceId: resource, HttpMethod: aws.String("GET"), StatusCode: aws.String("200"),
			ResponseTemplates: map[string]string{"application/json": body},
		}); err != nil {
			t.Fatal(err)
		}
	}
	mockGET(pets.Id, `"pets"`)
	for _, stage := range []string{"prod", "beta"} {
		if _, err = c.CreateDeployment(ctx, &restapi.CreateDeploymentInput{
			RestApiId: api.Id, StageName: aws.String(stage),
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.CreateDomainName(ctx, &restapi.CreateDomainNameInput{DomainName: aws.String("api.example.com")})
	code(t, err, "BadRequestException")

	domain, err := c.CreateDomainName(ctx, &restapi.CreateDomainNameInput{
		DomainName:             aws.String("api.example.com"),
		RegionalCertificateArn: aws.String("arn:aws:acm:us-east-1:000000000000:certificate/abc"),
		EndpointConfiguration:  &resttypes.EndpointConfiguration{Types: []resttypes.EndpointType{"REGIONAL"}},
	})
	if err != nil || aws.ToString(domain.RegionalDomainName) == "" || domain.DomainNameStatus != "AVAILABLE" {
		t.Fatalf("%+v %v", domain, err)
	}

	_, err = c.CreateDomainName(ctx, &restapi.CreateDomainNameInput{
		DomainName:     aws.String("api.example.com"),
		CertificateArn: aws.String("arn:aws:acm:us-east-1:000000000000:certificate/abc"),
	})
	code(t, err, "ConflictException")

	// The empty base path serves prod; /v2 serves the stage the path names.
	for _, m := range []*restapi.CreateBasePathMappingInput{
		{DomainName: domain.DomainName, RestApiId: api.Id, Stage: aws.String("prod")},
		{DomainName: domain.DomainName, RestApiId: api.Id, BasePath: aws.String("v2")},
	} {
		if _, err = c.CreateBasePathMapping(ctx, m); err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.CreateBasePathMapping(ctx, &restapi.CreateBasePathMappingInput{
		DomainName: domain.DomainName, RestApiId: api.Id, Stage: aws.String("missing"), BasePath: aws.String("x"),
	})
	code(t, err, "BadRequestException")

	mappings, err := c.GetBasePathMappings(ctx, &restapi.GetBasePathMappingsInput{DomainName: domain.DomainName})
	if err != nil || len(mappings.Items) != 2 || aws.ToString(mappings.Items[0].BasePath) != "(none)" {
		t.Fatalf("%+v %v", mappings, err)
	}

	for _, tc := range []struct{ url, host, body string }{
		{s.URL + "/_aws/domain/api.example.com/pets", "", `"pets"`},
		{s.URL + "/pets", "api.example.com", `"pets"`},
		{s.URL + "/v2/beta/pets", "api.example.com", `"pets"`},
	} {
		if status, body := getStatus(t, tc.url, tc.host); status != http.StatusOK || body != tc.body {
			t.Fatalf("%s (%s): %d %s", tc.url, tc.host, status, body)
		}
	}

	if _, err = c.DeleteBasePathMapping(ctx, &restapi.DeleteBasePathMappingInput{
		DomainName: domain.DomainName, BasePath: aws.String("(none)"),
	}); err != nil {
		t.Fatal(err)
	}

	if status, _ := getStatus(t, s.URL+"/pets", "api.example.com"); status != http.StatusForbidden {
		t.Fatalf("unmapped path: %d", status)
	}

	if _, err = c.DeleteDomainName(ctx, &restapi.DeleteDomainNameInput{DomainName: domain.DomainName}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetDomainName(ctx, &restapi.GetDomainNameInput{DomainName: domain.DomainName})
	code(t, err, "NotFoundException")
}
