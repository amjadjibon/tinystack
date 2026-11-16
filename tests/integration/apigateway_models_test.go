package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	restapi "github.com/aws/aws-sdk-go-v2/service/apigateway"
	resttypes "github.com/aws/aws-sdk-go-v2/service/apigateway/types"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
)

// Models are draft-4 JSON schemas that may refer to each other by URI; a request validator on a method checks
// required parameters and the body against the model for its content type before the integration runs.
func TestRESTAPIModelsAndValidation(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := restClient(s)
	respond := createFunction(t, lambdaClient(s, "test"), "respond", "app.respond")
	api, err := c.CreateRestApi(ctx, &restapi.CreateRestApiInput{Name: aws.String("pets")})
	if err != nil {
		t.Fatal(err)
	}

	id := aws.ToString(api.Id)
	models, err := c.GetModels(ctx, &restapi.GetModelsInput{RestApiId: api.Id})
	if err != nil || len(models.Items) != 2 || aws.ToString(models.Items[0].Name) != "Empty" ||
		aws.ToString(models.Items[1].Name) != "Error" {
		t.Fatalf("default models %+v, %v", models, err)
	}

	model := func(name, schema string) error {
		_, createErr := c.CreateModel(ctx, &restapi.CreateModelInput{
			RestApiId: api.Id, Name: aws.String(name), ContentType: aws.String("application/json"),
			Schema: aws.String(schema),
		})
		return createErr
	}

	if err = model(
		"Tag",
		`{"$schema":"http://json-schema.org/draft-04/schema#","type":"string","maxLength":5}`,
	); err != nil {
		t.Fatal(err)
	}

	pet := `{"$schema":"http://json-schema.org/draft-04/schema#","type":"object","required":["name"],
		"properties":{"name":{"type":"string"},"price":{"type":"number","minimum":0,"maximum":100,
		"exclusiveMaximum":true},"tag":{"$ref":"https://apigateway.amazonaws.com/restapis/` + id + `/models/Tag"},
		"id":{"type":"integer"}}}`
	if err = model("Pet", pet); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, schema, code string }{
		{"bad-name", `{}`, "BadRequestException"},
		{"Pet", `{}`, "ConflictException"},
		{"Broken", `{`, "BadRequestException"},
		{"Dangling", `{"$ref":"https://apigateway.amazonaws.com/restapis/` + id + `/models/Nope"}`, "BadRequestException"},
		{"Newer", `{"$schema":"https://json-schema.org/draft/2020-12/schema"}`, "BadRequestException"},
	} {
		code(t, model(tc.name, tc.schema), tc.code)
	}

	updated, err := c.UpdateModel(ctx, &restapi.UpdateModelInput{
		RestApiId: api.Id, ModelName: aws.String("Pet"),
		PatchOperations: []resttypes.PatchOperation{replace("/description", "a pet")},
	})
	if err != nil || aws.ToString(updated.Description) != "a pet" || aws.ToString(updated.Schema) != pet {
		t.Fatalf("updated %+v, %v", updated, err)
	}

	validator, err := c.CreateRequestValidator(ctx, &restapi.CreateRequestValidatorInput{
		RestApiId: api.Id, Name: aws.String("all"), ValidateRequestBody: true, ValidateRequestParameters: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateRequestValidator(ctx, &restapi.CreateRequestValidatorInput{
		RestApiId: api.Id, Name: aws.String("all"),
	})
	code(t, err, "ConflictException")

	pets, err := c.CreateResource(ctx, &restapi.CreateResourceInput{
		RestApiId: api.Id, ParentId: api.RootResourceId, PathPart: aws.String("pets"),
	})
	if err != nil {
		t.Fatal(err)
	}

	method := &restapi.PutMethodInput{
		RestApiId: api.Id, ResourceId: pets.Id, HttpMethod: aws.String("POST"), AuthorizationType: aws.String("NONE"),
		RequestModels: map[string]string{"application/json": "Missing"},
	}
	_, err = c.PutMethod(ctx, method)
	code(t, err, "BadRequestException")

	method.RequestModels, method.RequestValidatorId = map[string]string{"application/json": "Pet"}, aws.String("nope")
	_, err = c.PutMethod(ctx, method)
	code(t, err, "BadRequestException")

	method.RequestValidatorId = validator.Id
	method.RequestParameters = map[string]bool{
		"method.request.querystring.store": true, "method.request.header.X-Trace": true,
		"method.request.querystring.page": false,
	}
	put, err := c.PutMethod(ctx, method)
	if err != nil || aws.ToString(put.RequestValidatorId) != aws.ToString(validator.Id) {
		t.Fatalf("put %+v, %v", put, err)
	}

	if _, err = c.PutIntegration(ctx, &restapi.PutIntegrationInput{
		RestApiId: api.Id, ResourceId: pets.Id, HttpMethod: aws.String("POST"),
		Type: resttypes.IntegrationTypeAwsProxy, IntegrationHttpMethod: aws.String("POST"),
		Uri: lambdaURI(respond.FunctionArn),
	}); err != nil {
		t.Fatal(err)
	}

	// Models and validators in use cannot be deleted.
	_, err = c.DeleteModel(ctx, &restapi.DeleteModelInput{RestApiId: api.Id, ModelName: aws.String("Pet")})
	code(t, err, "ConflictException")
	_, err = c.DeleteModel(ctx, &restapi.DeleteModelInput{RestApiId: api.Id, ModelName: aws.String("Tag")})
	code(t, err, "ConflictException")
	_, err = c.DeleteRequestValidator(ctx, &restapi.DeleteRequestValidatorInput{
		RestApiId: api.Id, RequestValidatorId: validator.Id,
	})
	code(t, err, "ConflictException")

	if _, err = c.CreateDeployment(ctx, &restapi.CreateDeploymentInput{
		RestApiId: api.Id, StageName: aws.String("prod"),
	}); err != nil {
		t.Fatal(err)
	}

	trace := map[string]string{"X-Trace": "1", "Content-Type": "application/json"}
	plain := map[string]string{"X-Trace": "1", "Content-Type": "text/plain"}
	for name, tc := range map[string]struct {
		header              map[string]string
		path, body, message string
		status              int
	}{
		"missing":   {nil, "/prod/pets", `{"name":"rex"}`, "Missing required request parameters: [X-Trace, store]", 400},
		"blank":     {trace, "/prod/pets?store=%20", `{"name":"rex"}`, "Missing required request parameters: [store]", 400},
		"required":  {trace, "/prod/pets?store=1", `{"price":5}`, "Invalid request body", 400},
		"exclusive": {trace, "/prod/pets?store=1", `{"name":"rex","price":100}`, "Invalid request body", 400},
		"ref":       {trace, "/prod/pets?store=1", `{"name":"rex","tag":"toolong"}`, "Invalid request body", 400},
		"integer":   {trace, "/prod/pets?store=1", `{"name":"rex","id":1.5}`, "Invalid request body", 400},
		"json":      {trace, "/prod/pets?store=1", `{"name":`, "Invalid request body", 400},
		"valid":     {trace, "/prod/pets?store=1", `{"name":"rex","price":99.5,"tag":"dog","id":3}`, "", 201},
		"untyped":   {map[string]string{"X-Trace": "1"}, "/prod/pets?store=1", `{"price":5}`, "Invalid request body", 400},
		"unmodeled": {plain, "/prod/pets?store=1", "plain", "", 201},
	} {
		status, header, body := call(t, s, "POST", id, tc.path, tc.header, tc.body)
		if status != tc.status || !strings.Contains(body, tc.message) {
			t.Fatalf("%s: %d %s", name, status, body)
		}

		if status == http.StatusBadRequest && header.Get("X-Amzn-Errortype") != "BadRequestException" {
			t.Fatalf("%s: header %v", name, header)
		}
	}

	validators, err := c.GetRequestValidators(ctx, &restapi.GetRequestValidatorsInput{RestApiId: api.Id})
	if err != nil || len(validators.Items) != 1 || !validators.Items[0].ValidateRequestBody {
		t.Fatalf("validators %+v, %v", validators, err)
	}

	changed, err := c.UpdateRequestValidator(ctx, &restapi.UpdateRequestValidatorInput{
		RestApiId: api.Id, RequestValidatorId: validator.Id,
		PatchOperations: []resttypes.PatchOperation{replace("/validateRequestBody", "false")},
	})
	if err != nil || changed.ValidateRequestBody || !changed.ValidateRequestParameters {
		t.Fatalf("changed %+v, %v", changed, err)
	}

	// Removing the method's model and validator frees them.
	if _, err = c.UpdateMethod(ctx, &restapi.UpdateMethodInput{
		RestApiId: api.Id, ResourceId: pets.Id, HttpMethod: aws.String("POST"),
		PatchOperations: []resttypes.PatchOperation{
			{Op: resttypes.OpRemove, Path: aws.String("/requestModels/application~1json")},
			{Op: resttypes.OpRemove, Path: aws.String("/requestValidatorId")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"Pet", "Tag"} {
		if _, err = c.DeleteModel(ctx, &restapi.DeleteModelInput{
			RestApiId: api.Id, ModelName: aws.String(name),
		}); err != nil {
			t.Fatal(name, err)
		}
	}

	if _, err = c.DeleteRequestValidator(ctx, &restapi.DeleteRequestValidatorInput{
		RestApiId: api.Id, RequestValidatorId: validator.Id,
	}); err != nil {
		t.Fatal(err)
	}

	models, err = c.GetModels(ctx, &restapi.GetModelsInput{RestApiId: api.Id})
	names := []string{}
	for _, m := range models.Items {
		names = append(names, aws.ToString(m.Name))
	}

	if err != nil || !slices.Equal(names, []string{"Empty", "Error"}) {
		t.Fatalf("models %v, %v", names, err)
	}

	// The deployed stage keeps the validator it was deployed with.
	if status, _, body := call(t, s, "POST", id, "/prod/pets", nil, "{}"); status != http.StatusBadRequest {
		t.Fatalf("deployed: %d %s", status, body)
	}
}

const modelsTemplate = `AWSTemplateFormatVersion: "2010-09-09"
Parameters:
  Name: {Type: String, Default: rex}
Resources:
  Role:
    Type: AWS::IAM::Role
    Properties:
      AssumeRolePolicyDocument:
        Version: "2012-10-17"
        Statement:
          - {Effect: Allow, Principal: {Service: lambda.amazonaws.com}, Action: sts:AssumeRole}
  Function:
    Type: AWS::Lambda::Function
    Properties:
      Runtime: python3.12
      Handler: app.respond
      Role: !GetAtt Role.Arn
      Code: {ZipFile: "def respond(event, context): pass"}
  Rest:
    Type: AWS::ApiGateway::RestApi
    Properties:
      Name: !Sub "${AWS::StackName}-rest"
  Pets:
    Type: AWS::ApiGateway::Resource
    Properties:
      RestApiId: !Ref Rest
      ParentId: !GetAtt Rest.RootResourceId
      PathPart: pets
  Pet:
    Type: AWS::ApiGateway::Model
    Properties:
      RestApiId: !Ref Rest
      ContentType: application/json
      Schema:
        $schema: http://json-schema.org/draft-04/schema#
        type: object
        required: [name]
        properties:
          name: {type: string, enum: [!Ref Name]}
  Validator:
    Type: AWS::ApiGateway::RequestValidator
    Properties:
      RestApiId: !Ref Rest
      ValidateRequestBody: true
  Post:
    Type: AWS::ApiGateway::Method
    Properties:
      RestApiId: !Ref Rest
      ResourceId: !Ref Pets
      HttpMethod: POST
      AuthorizationType: NONE
      RequestModels: {application/json: !Ref Pet}
      RequestValidatorId: !Ref Validator
      Integration:
        Type: AWS_PROXY
        IntegrationHttpMethod: POST
        Uri: !Sub "arn:aws:apigateway:${AWS::Region}:lambda:path/2015-03-31/functions/${Function.Arn}/invocations"
  Deployment:
    Type: AWS::ApiGateway::Deployment
    DependsOn: Post
    Properties:
      RestApiId: !Ref Rest
      StageName: prod
  Invoke:
    Type: AWS::Lambda::Permission
    Properties:
      FunctionName: !Ref Function
      Action: lambda:InvokeFunction
      Principal: apigateway.amazonaws.com
Outputs:
  Rest: {Value: !Ref Rest}
  Model: {Value: !Ref Pet}
  Validator: {Value: !GetAtt Validator.RequestValidatorId}
`

func TestCloudFormationRESTModels(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cfnClient(s)
	rest := restClient(s)
	if _, err := c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String("models"), TemplateBody: aws.String(modelsTemplate),
		Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityIam},
	}); err != nil {
		t.Fatal(err)
	}

	st := wantStatus(ctx, t, c, "models", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&st)
	if outputs["Model"] == "" || strings.Contains(outputs["Model"], "-") || outputs["Validator"] == "" {
		t.Fatalf("outputs %v", outputs)
	}

	for body, want := range map[string]int{`{"name":"rex"}`: 201, `{"name":"max"}`: 400, `{}`: 400} {
		if status, _, got := call(t, s, http.MethodPost, outputs["Rest"], "/prod/pets", nil, body); status != want {
			t.Fatalf("%s: %d %s", body, status, got)
		}
	}

	// A changed schema updates the model in place.
	if _, err := c.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName: aws.String("models"), TemplateBody: aws.String(modelsTemplate),
		Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityIam},
		Parameters:   []cfntypes.Parameter{{ParameterKey: aws.String("Name"), ParameterValue: aws.String("max")}},
	}); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "models", cfntypes.StackStatusUpdateComplete)
	model, err := rest.GetModel(ctx, &restapi.GetModelInput{
		RestApiId: aws.String(outputs["Rest"]), ModelName: aws.String(outputs["Model"]),
	})
	if err != nil || !strings.Contains(aws.ToString(model.Schema), `"enum":["max"]`) {
		t.Fatalf("model %+v, %v", model, err)
	}

	if _, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: aws.String("models")}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "stack models to be deleted", func() bool {
		_, describeErr := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: aws.String("models")})
		return describeErr != nil
	})
}

// OpenAPI import turns Swagger definitions or component schemas into models, body schemas into method request
// models and the request validator extensions into validators; export writes them back.
func TestRESTAPIOpenAPIModels(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := restClient(s)
	uri := *lambdaURI(createFunction(t, lambdaClient(s, "test"), "respond", "app.respond").FunctionArn)
	integration := fmt.Sprintf(`"x-amazon-apigateway-integration":{"type":"aws_proxy","httpMethod":"POST","uri":%q}`,
		uri)
	validators := `"x-amazon-apigateway-request-validators":{
		"all":{"validateRequestBody":true,"validateRequestParameters":true},
		"params":{"validateRequestBody":false,"validateRequestParameters":true}},
		"x-amazon-apigateway-request-validator":"all"`
	pet := `{"type":"object","required":["name"],"properties":{"name":{"type":"string"},"tag":{"$ref":%q}}}`
	tag := `{"type":"string","maxLength":3}`
	specs := map[string]string{
		"swagger": fmt.Sprintf(`{"swagger":"2.0","info":{"title":"pets","version":"1"},%s,
			"definitions":{"Pet":`+pet+`,"Tag":%s},
			"paths":{"/pets":{
				"post":{"responses":{},"parameters":[
					{"in":"query","name":"store","required":true,"type":"string"},
					{"in":"body","name":"Pet","schema":{"$ref":"#/definitions/Pet"}}],%s},
				"put":{"responses":{},"x-amazon-apigateway-request-validator":"params","parameters":[
					{"in":"query","name":"store","required":true,"type":"string"},
					{"in":"body","name":"Pet","schema":{"$ref":"#/definitions/Pet"}}],%s}}}}`,
			validators, "#/definitions/Tag", tag, integration, integration),
		"oas30": fmt.Sprintf(`{"openapi":"3.0.1","info":{"title":"pets","version":"1"},%s,
			"components":{"schemas":{"Pet":`+pet+`,"Tag":%s}},
			"paths":{"/pets":{
				"post":{"responses":{},"parameters":[{"in":"query","name":"store","required":true}],
					"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Pet"}}}},%s},
				"put":{"responses":{},"x-amazon-apigateway-request-validator":"params",
					"parameters":[{"in":"query","name":"store","required":true}],
					"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Pet"}}}},%s}}}}`,
			validators, "#/components/schemas/Tag", tag, integration, integration),
	}

	check := func(t *testing.T, id *string) {
		t.Helper()
		if _, err := c.CreateDeployment(context.Background(), &restapi.CreateDeploymentInput{
			RestApiId: id, StageName: aws.String("prod"),
		}); err != nil {
			t.Fatal(err)
		}

		for _, tc := range []struct {
			method, path, body string
			status             int
		}{
			{"POST", "/prod/pets", `{"name":"rex"}`, 400},
			{"POST", "/prod/pets?store=1", `{"tag":"dog"}`, 400},
			{"POST", "/prod/pets?store=1", `{"name":"rex","tag":"long"}`, 400},
			{"POST", "/prod/pets?store=1", `{"name":"rex","tag":"dog"}`, 201},
			{"PUT", "/prod/pets", `{}`, 400},
			{"PUT", "/prod/pets?store=1", `{}`, 201},
		} {
			status, _, body := call(t, s, tc.method, *id, tc.path, nil, tc.body)
			if status != tc.status {
				t.Fatalf("%s %s %s: %d %s", tc.method, tc.path, tc.body, status, body)
			}
		}
	}

	for format, spec := range specs {
		t.Run(format, func(t *testing.T) {
			imported, err := c.ImportRestApi(ctx, &restapi.ImportRestApiInput{Body: []byte(spec)})
			if err != nil {
				t.Fatal(err)
			}

			check(t, imported.Id)
			model, err := c.GetModel(ctx, &restapi.GetModelInput{RestApiId: imported.Id, ModelName: aws.String("Pet")})
			if err != nil || !strings.Contains(aws.ToString(model.Schema), "/restapis/"+*imported.Id+"/models/Tag") {
				t.Fatalf("model %+v, %v", model, err)
			}

			exported, err := c.GetExport(ctx, &restapi.GetExportInput{
				RestApiId: imported.Id, StageName: aws.String("prod"), ExportType: aws.String(format),
				Parameters: map[string]string{"extensions": "apigateway"},
			})
			if err != nil {
				t.Fatal(err)
			}

			copied, err := c.ImportRestApi(ctx, &restapi.ImportRestApiInput{Body: exported.Body})
			if err != nil {
				t.Fatalf("%v\n%s", err, exported.Body)
			}

			check(t, copied.Id)
		})
	}

	for name, spec := range map[string]string{
		"inline": `{"openapi":"3.0.1","info":{"title":"x","version":"1"},"paths":{"/a":{"post":{"responses":{},
			"requestBody":{"content":{"application/json":{"schema":{"type":"object"}}}}}}}}`,
		"validator": `{"openapi":"3.0.1","info":{"title":"x","version":"1"},
			"x-amazon-apigateway-request-validator":"missing","paths":{"/a":{"get":{"responses":{}}}}}`,
		"model": `{"openapi":"3.0.1","info":{"title":"x","version":"1"},"paths":{"/a":{"post":{"responses":{},
			"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Nope"}}}}}}}}`,
		"ref": `{"openapi":"3.0.1","info":{"title":"x","version":"1"},"paths":{},
			"components":{"schemas":{"A":{"$ref":"#/components/schemas/B"}}}}`,
		"name": `{"openapi":"3.0.1","info":{"title":"x","version":"1"},"paths":{},
			"components":{"schemas":{"my-model":{"type":"object"}}}}`,
	} {
		_, err := c.ImportRestApi(ctx, &restapi.ImportRestApiInput{Body: []byte(spec)})
		code(t, err, "BadRequestException")
		if t.Failed() {
			t.Fatal(name)
		}
	}
}
