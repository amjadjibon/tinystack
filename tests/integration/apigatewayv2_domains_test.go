package integration_test

import (
	"encoding/json"
	"net/http"
	"testing"

	restapi "github.com/aws/aws-sdk-go-v2/service/apigateway"
	apigw "github.com/aws/aws-sdk-go-v2/service/apigatewayv2"
	apigwtypes "github.com/aws/aws-sdk-go-v2/service/apigatewayv2/types"
)

func TestHTTPAPICustomDomains(t *testing.T) {
	s := server(t)
	c := apigwClient(s)
	ctx := t.Context()
	a, err := c.CreateApi(ctx, &apigw.CreateApiInput{
		Name: new("mapped"), ProtocolType: apigwtypes.ProtocolTypeHttp, DisableExecuteApiEndpoint: new(true),
	})
	mustAuthorize(t, err)
	fn := createFunction(t, lambdaClient(s, "test"), "mapped", "app.respond")
	integration, err := c.CreateIntegration(ctx, &apigw.CreateIntegrationInput{
		ApiId: a.ApiId, IntegrationType: apigwtypes.IntegrationTypeAwsProxy,
		IntegrationUri: fn.FunctionArn, PayloadFormatVersion: new("2.0"),
	})
	mustAuthorize(t, err)
	_, err = c.CreateRoute(ctx, &apigw.CreateRouteInput{
		ApiId: a.ApiId, RouteKey: new("$default"), Target: new("integrations/" + *integration.IntegrationId),
	})
	mustAuthorize(t, err)
	for _, stage := range []string{"$default", "prod", "beta"} {
		_, err = c.CreateStage(
			ctx,
			&apigw.CreateStageInput{ApiId: a.ApiId, StageName: new(stage), AutoDeploy: new(true)},
		)
		mustAuthorize(t, err)
	}

	host := "mapped.example.com"
	_, err = c.CreateDomainName(ctx, &apigw.CreateDomainNameInput{
		DomainName: new(host), MutualTlsAuthentication: &apigwtypes.MutualTlsAuthenticationInput{
			TruststoreUri: new("s3://example/truststore.pem"),
		},
	})
	code(t, err, "NotImplemented")
	domain, err := c.CreateDomainName(ctx, &apigw.CreateDomainNameInput{DomainName: new("Mapped.Example.com")})
	mustAuthorize(t, err)
	if *domain.DomainName != host {
		t.Fatalf("domain name = %v, want %s", *domain.DomainName, host)
	}

	_, err = restClient(s).CreateDomainName(ctx, &restapi.CreateDomainNameInput{
		DomainName: domain.DomainName, CertificateArn: new("arn:aws:acm:us-east-1:000000000000:certificate/example"),
	})
	code(t, err, "ConflictException")
	create := func(key, stage string) *string {
		t.Helper()
		mapping, mappingErr := c.CreateApiMapping(ctx, &apigw.CreateApiMappingInput{
			DomainName: domain.DomainName, ApiId: a.ApiId, ApiMappingKey: new(key), Stage: new(stage),
		})
		mustAuthorize(t, mappingErr)
		return mapping.ApiMappingId
	}
	root := create("", "$default")
	v1 := create("v1", "prod")
	multi := create("v1/internal", "beta")
	_, err = c.CreateApiMapping(ctx, &apigw.CreateApiMappingInput{
		DomainName: domain.DomainName, ApiId: a.ApiId, ApiMappingKey: new("v1"), Stage: new("prod"),
	})
	code(t, err, "ConflictException")

	for _, tc := range []struct{ path, stage, rawPath string }{
		{"/beta/pets", "$default", "/beta/pets"},
		{"/v1/pets", "prod", "/pets"},
		{"/v1/internal/pets", "beta", "/pets"},
		{"/v1alpha/pets", "prod", "/alpha/pets"},
	} {
		status, body := getStatus(t, s.URL+tc.path, host)
		var event struct {
			RawPath        string `json:"rawPath"`
			RequestContext struct {
				Stage      string `json:"stage"`
				DomainName string `json:"domainName"`
			} `json:"requestContext"`
		}
		if status != http.StatusCreated || json.Unmarshal([]byte(body), &event) != nil ||
			event.RawPath != tc.rawPath || event.RequestContext.Stage != tc.stage || event.RequestContext.DomainName != host {
			t.Fatalf("%s = %d %s, want stage %s path %s", tc.path, status, body, tc.stage, tc.rawPath)
		}
	}

	status, _, _ := call(t, s, http.MethodGet, *a.ApiId, "/prod/pets", nil, "")
	if status != http.StatusForbidden {
		t.Fatalf("default endpoint status = %d, want 403", status)
	}

	_, err = c.DeleteStage(ctx, &apigw.DeleteStageInput{ApiId: a.ApiId, StageName: new("prod")})
	code(t, err, "ConflictException")
	_, err = c.UpdateApiMapping(ctx, &apigw.UpdateApiMappingInput{
		ApiMappingId: v1, ApiId: a.ApiId, DomainName: domain.DomainName, ApiMappingKey: new("renamed"),
	})
	mustAuthorize(t, err)
	got, err := c.GetApiMapping(ctx, &apigw.GetApiMappingInput{DomainName: domain.DomainName, ApiMappingId: v1})
	mustAuthorize(t, err)
	if *got.ApiMappingKey != "renamed" {
		t.Fatalf("mapping = %+v", got)
	}

	items, err := c.GetApiMappings(ctx, &apigw.GetApiMappingsInput{
		DomainName: domain.DomainName, MaxResults: new("1"),
	})
	mustAuthorize(t, err)
	if len(items.Items) != 1 || items.NextToken == nil {
		t.Fatalf("expected first page of mappings, got %+v", items)
	}

	for _, id := range []*string{root, v1, multi} {
		_, err = c.DeleteApiMapping(ctx, &apigw.DeleteApiMappingInput{DomainName: domain.DomainName, ApiMappingId: id})
		mustAuthorize(t, err)
	}

	if status, _ := getStatus(t, s.URL+"/pets", host); status != http.StatusForbidden {
		t.Fatalf("unmapped domain status = %d, want 403", status)
	}

	_, err = c.DeleteDomainName(ctx, &apigw.DeleteDomainNameInput{DomainName: domain.DomainName})
	mustAuthorize(t, err)
	_, err = c.GetDomainName(ctx, &apigw.GetDomainNameInput{DomainName: domain.DomainName})
	code(t, err, "NotFoundException")
}
