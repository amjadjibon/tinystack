package integration_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	restapi "github.com/aws/aws-sdk-go-v2/service/apigateway"
	resttypes "github.com/aws/aws-sdk-go-v2/service/apigateway/types"
)

func TestRESTAPIKeysAndUsagePlans(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := restClient(s)
	api, err := c.CreateRestApi(ctx, &restapi.CreateRestApiInput{Name: aws.String("metered")})
	if err != nil {
		t.Fatal(err)
	}

	id := aws.ToString(api.Id)
	if _, err = c.PutMethod(ctx, &restapi.PutMethodInput{
		RestApiId: api.Id, ResourceId: api.RootResourceId, HttpMethod: aws.String("GET"),
		AuthorizationType: aws.String("NONE"), ApiKeyRequired: true,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.PutIntegration(ctx, &restapi.PutIntegrationInput{
		RestApiId:        api.Id,
		ResourceId:       api.RootResourceId,
		HttpMethod:       aws.String("GET"),
		Type:             resttypes.IntegrationTypeMock,
		RequestTemplates: map[string]string{"application/json": `{"statusCode": 200}`},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.PutMethodResponse(ctx, &restapi.PutMethodResponseInput{
		RestApiId: api.Id, ResourceId: api.RootResourceId, HttpMethod: aws.String("GET"), StatusCode: aws.String("200"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.PutIntegrationResponse(ctx, &restapi.PutIntegrationResponseInput{
		RestApiId: api.Id, ResourceId: api.RootResourceId, HttpMethod: aws.String("GET"), StatusCode: aws.String("200"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreateDeployment(ctx, &restapi.CreateDeploymentInput{
		RestApiId: api.Id, StageName: aws.String("prod"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateApiKey(ctx, &restapi.CreateApiKeyInput{Name: aws.String("short"), Value: aws.String("tiny")})
	code(t, err, "BadRequestException")

	key, err := c.CreateApiKey(ctx, &restapi.CreateApiKeyInput{
		Name: aws.String("partner"), Enabled: true, CustomerId: aws.String("acme"),
		Value: aws.String("partner-key-0123456789"),
	})
	if err != nil || aws.ToString(key.Value) != "partner-key-0123456789" || !key.Enabled {
		t.Fatalf("%+v %v", key, err)
	}

	_, err = c.CreateApiKey(ctx, &restapi.CreateApiKeyInput{
		Name: aws.String("again"), Value: aws.String("partner-key-0123456789"),
	})
	code(t, err, "ConflictException")

	got, err := c.GetApiKey(ctx, &restapi.GetApiKeyInput{ApiKey: key.Id})
	if err != nil || got.Value != nil || aws.ToString(got.CustomerId) != "acme" {
		t.Fatalf("%+v %v", got, err)
	}

	_, err = c.CreateUsagePlan(ctx, &restapi.CreateUsagePlanInput{
		Name:      aws.String("bad"),
		ApiStages: []resttypes.ApiStage{{ApiId: api.Id, Stage: aws.String("missing")}},
	})
	code(t, err, "BadRequestException")

	plan, err := c.CreateUsagePlan(ctx, &restapi.CreateUsagePlanInput{
		Name:     aws.String("gold"),
		Throttle: &resttypes.ThrottleSettings{BurstLimit: 10, RateLimit: 5},
		Quota:    &resttypes.QuotaSettings{Limit: 1000, Period: resttypes.QuotaPeriodTypeMonth},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Without a key, with a key in no plan for the stage, and with one that
	// is, the method answers 403, 403, and 200.
	status, _, _ := call(t, s, "GET", id, "/prod", nil, "")
	if status != http.StatusForbidden {
		t.Fatalf("no key: %d", status)
	}

	if _, err = c.CreateUsagePlanKey(ctx, &restapi.CreateUsagePlanKeyInput{
		UsagePlanId: plan.Id, KeyId: key.Id, KeyType: aws.String("API_KEY"),
	}); err != nil {
		t.Fatal(err)
	}

	withKey := map[string]string{"x-api-key": "partner-key-0123456789"}
	if status, _, _ = call(t, s, "GET", id, "/prod", withKey, ""); status != http.StatusForbidden {
		t.Fatalf("no stage: %d", status)
	}

	updated, err := c.UpdateUsagePlan(ctx, &restapi.UpdateUsagePlanInput{
		UsagePlanId: plan.Id, PatchOperations: []resttypes.PatchOperation{
			{Op: resttypes.OpAdd, Path: aws.String("/apiStages"), Value: aws.String(id + ":prod")},
			replace("/throttle/rateLimit", "50"),
		},
	})
	if err != nil || len(updated.ApiStages) != 1 || updated.Throttle.RateLimit != 50 {
		t.Fatalf("%+v %v", updated, err)
	}

	if status, _, _ = call(t, s, "GET", id, "/prod", withKey, ""); status != http.StatusOK {
		t.Fatalf("with key: %d", status)
	}

	// A disabled key is refused.
	if _, err = c.UpdateApiKey(ctx, &restapi.UpdateApiKeyInput{
		ApiKey: key.Id, PatchOperations: []resttypes.PatchOperation{replace("/enabled", "false")},
	}); err != nil {
		t.Fatal(err)
	}

	if status, _, _ = call(t, s, "GET", id, "/prod", withKey, ""); status != http.StatusForbidden {
		t.Fatalf("disabled key: %d", status)
	}

	plans, err := c.GetUsagePlans(ctx, &restapi.GetUsagePlansInput{KeyId: key.Id})
	if err != nil || len(plans.Items) != 1 {
		t.Fatalf("%+v %v", plans, err)
	}

	planKeys, err := c.GetUsagePlanKeys(ctx, &restapi.GetUsagePlanKeysInput{UsagePlanId: plan.Id})
	if err != nil || len(planKeys.Items) != 1 || aws.ToString(planKeys.Items[0].Type) != "API_KEY" {
		t.Fatalf("%+v %v", planKeys, err)
	}

	keys, err := c.GetApiKeys(
		ctx,
		&restapi.GetApiKeysInput{IncludeValues: aws.Bool(true), NameQuery: aws.String("part")},
	)
	if err != nil || len(keys.Items) != 1 || aws.ToString(keys.Items[0].Value) != "partner-key-0123456789" {
		t.Fatalf("%+v %v", keys, err)
	}

	// Deleting the key takes it out of its plan.
	if _, err = c.DeleteApiKey(ctx, &restapi.DeleteApiKeyInput{ApiKey: key.Id}); err != nil {
		t.Fatal(err)
	}

	planKeys, err = c.GetUsagePlanKeys(ctx, &restapi.GetUsagePlanKeysInput{UsagePlanId: plan.Id})
	if err != nil || len(planKeys.Items) != 0 {
		t.Fatalf("%+v %v", planKeys, err)
	}

	if _, err = c.DeleteUsagePlan(ctx, &restapi.DeleteUsagePlanInput{UsagePlanId: plan.Id}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetUsagePlan(ctx, &restapi.GetUsagePlanInput{UsagePlanId: plan.Id})
	code(t, err, "NotFoundException")
}
