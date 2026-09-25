package integration_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// invokeGreeting invokes the echo function named by name and returns the
// executed version and the GREETING it saw.
func invokeGreeting(t *testing.T, c *sdklambda.Client, name string, qualifier *string) (version, greeting string) {
	t.Helper()
	out, err := c.Invoke(context.Background(), &sdklambda.InvokeInput{
		FunctionName: aws.String(name),
		Qualifier:    qualifier,
		Payload:      []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	var body struct {
		Env map[string]string `json:"env"`
	}
	if err = json.Unmarshal(out.Payload, &body); err != nil {
		t.Fatalf("%s %v", out.Payload, err)
	}

	if body.Env["AWS_LAMBDA_FUNCTION_VERSION"] != aws.ToString(out.ExecutedVersion) {
		t.Fatalf("%v ran %s", body.Env, aws.ToString(out.ExecutedVersion))
	}

	return aws.ToString(out.ExecutedVersion), body.Env["GREETING"]
}

func setGreeting(t *testing.T, c *sdklambda.Client, greeting string) {
	t.Helper()
	_, err := c.UpdateFunctionConfiguration(context.Background(), &sdklambda.UpdateFunctionConfigurationInput{
		FunctionName: aws.String("worker"),
		Environment:  &lambdatypes.Environment{Variables: map[string]string{"GREETING": greeting}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLambdaVersionsAndAliases(t *testing.T) {
	runtime := &fakeRuntime{calls: make(chan invocation, 100)}
	s := serverWith(t, runtime)
	c := lambdaClient(s, "test")
	ctx := context.Background()
	arn := "arn:aws:lambda:us-east-1:000000000000:function:worker"
	created, err := c.CreateFunction(ctx, &sdklambda.CreateFunctionInput{
		FunctionName: aws.String("worker"),
		Runtime:      lambdatypes.RuntimePython312,
		Role:         aws.String("arn:aws:iam::000000000000:role/lambda"),
		Handler:      aws.String("app.echo"),
		Code:         &lambdatypes.FunctionCode{ZipFile: zipOf(t, "app.py", "x = 1\n")},
		Environment:  &lambdatypes.Environment{Variables: map[string]string{"GREETING": "v1"}},
		Tags:         map[string]string{"team": "core"},
		Publish:      true,
	})
	if err != nil || aws.ToString(created.Version) != "1" || aws.ToString(created.FunctionArn) != arn+":1" {
		t.Fatalf("%+v %v", created, err)
	}

	// A version keeps the configuration it was published with.
	setGreeting(t, c, "v2")
	second, err := c.PublishVersion(ctx, &sdklambda.PublishVersionInput{
		FunctionName: aws.String("worker"),
		Description:  aws.String("second"),
	})
	if err != nil || aws.ToString(second.Version) != "2" || aws.ToString(second.Description) != "second" {
		t.Fatalf("%+v %v", second, err)
	}

	// Publishing unchanged code and configuration returns the last version.
	again, err := c.PublishVersion(ctx, &sdklambda.PublishVersionInput{FunctionName: aws.String("worker")})
	if err != nil || aws.ToString(again.Version) != "2" {
		t.Fatalf("%+v %v", again, err)
	}

	_, err = c.PublishVersion(ctx, &sdklambda.PublishVersionInput{
		FunctionName: aws.String("worker"),
		CodeSha256:   aws.String("stale"),
	})
	if got, _ := lambdaCode(err); got != "InvalidParameterValueException" {
		t.Fatalf("code hash: %v", err)
	}

	_, err = c.PublishVersion(ctx, &sdklambda.PublishVersionInput{
		FunctionName: aws.String("worker"),
		RevisionId:   aws.String("stale"),
	})
	if got, _ := lambdaCode(err); got != "PreconditionFailedException" {
		t.Fatalf("revision: %v", err)
	}

	setGreeting(t, c, "v3")
	got, err := c.GetFunction(ctx, &sdklambda.GetFunctionInput{
		FunctionName: aws.String("worker"),
		Qualifier:    aws.String("1"),
	})
	if err != nil || aws.ToString(got.Configuration.Version) != "1" ||
		got.Configuration.Environment.Variables["GREETING"] != "v1" || got.Tags["team"] != "core" {
		t.Fatalf("%+v %v", got, err)
	}

	for _, tc := range []struct {
		name      string
		qualifier *string
		version   string
		greeting  string
	}{
		{"worker", nil, "$LATEST", "v3"},
		{"worker:1", nil, "1", "v1"},
		{"worker", aws.String("2"), "2", "v2"},
		{arn + ":$LATEST", nil, "$LATEST", "v3"},
	} {
		if version, greeting := invokeGreeting(t, c, tc.name, tc.qualifier); version != tc.version ||
			greeting != tc.greeting {
			t.Fatalf("%s %v: ran %s with %s", tc.name, tc.qualifier, version, greeting)
		}
	}

	_, err = c.Invoke(ctx, &sdklambda.InvokeInput{FunctionName: aws.String("worker:1"), Qualifier: aws.String("2")})
	if got, _ := lambdaCode(err); got != "InvalidParameterValueException" {
		t.Fatalf("conflicting qualifiers: %v", err)
	}

	_, err = c.Invoke(ctx, &sdklambda.InvokeInput{FunctionName: aws.String("worker:9")})
	if got, _ := lambdaCode(err); got != "ResourceNotFoundException" {
		t.Fatalf("missing version: %v", err)
	}

	// An alias runs the version it points to.
	live, err := c.CreateAlias(ctx, &sdklambda.CreateAliasInput{
		FunctionName:    aws.String("worker"),
		Name:            aws.String("live"),
		FunctionVersion: aws.String("1"),
		Description:     aws.String("production"),
	})
	if err != nil || aws.ToString(live.AliasArn) != arn+":live" || aws.ToString(live.FunctionVersion) != "1" ||
		aws.ToString(live.RevisionId) == "" {
		t.Fatalf("%+v %v", live, err)
	}

	if version, greeting := invokeGreeting(t, c, "worker:live", nil); version != "1" || greeting != "v1" {
		t.Fatalf("alias ran %s with %s", version, greeting)
	}

	_, err = c.UpdateAlias(ctx, &sdklambda.UpdateAliasInput{
		FunctionName:    aws.String("worker"),
		Name:            aws.String("live"),
		FunctionVersion: aws.String("2"),
		RevisionId:      aws.String("stale"),
	})
	if got, _ := lambdaCode(err); got != "PreconditionFailedException" {
		t.Fatalf("stale alias revision: %v", err)
	}

	moved, err := c.UpdateAlias(ctx, &sdklambda.UpdateAliasInput{
		FunctionName:    aws.String("worker"),
		Name:            aws.String("live"),
		FunctionVersion: aws.String("2"),
		RevisionId:      live.RevisionId,
	})
	if err != nil || aws.ToString(moved.FunctionVersion) != "2" || aws.ToString(moved.Description) != "production" ||
		aws.ToString(moved.RevisionId) == aws.ToString(live.RevisionId) {
		t.Fatalf("%+v %v", moved, err)
	}

	if version, greeting := invokeGreeting(t, c, "worker", aws.String("live")); version != "2" || greeting != "v2" {
		t.Fatalf("moved alias ran %s with %s", version, greeting)
	}

	cfg, err := c.GetFunctionConfiguration(ctx, &sdklambda.GetFunctionConfigurationInput{
		FunctionName: aws.String("worker"),
		Qualifier:    aws.String("live"),
	})
	if err != nil || aws.ToString(cfg.Version) != "2" || cfg.Environment.Variables["GREETING"] != "v2" {
		t.Fatalf("%+v %v", cfg, err)
	}

	// Routing sends a share of the alias's invocations to one other version.
	route := func(weights map[string]float64) error {
		_, routeErr := c.UpdateAlias(ctx, &sdklambda.UpdateAliasInput{
			FunctionName:  aws.String("worker"),
			Name:          aws.String("live"),
			RoutingConfig: &lambdatypes.AliasRoutingConfiguration{AdditionalVersionWeights: weights},
		})
		return routeErr
	}

	if err = route(map[string]float64{"1": 1}); err != nil {
		t.Fatal(err)
	}

	if version, _ := invokeGreeting(t, c, "worker:live", nil); version != "1" {
		t.Fatalf("fully shifted alias ran %s", version)
	}

	routed, err := c.GetAlias(
		ctx,
		&sdklambda.GetAliasInput{FunctionName: aws.String("worker"), Name: aws.String("live")},
	)
	if err != nil || routed.RoutingConfig.AdditionalVersionWeights["1"] != 1 {
		t.Fatalf("%+v %v", routed, err)
	}

	for _, weights := range []map[string]float64{{"$LATEST": 0.5}, {"2": 0.5}, {"1": 1.5}, {"9": 0.5}} {
		if err = route(weights); err == nil {
			t.Fatalf("routing %v was accepted", weights)
		}
	}

	if err = route(map[string]float64{}); err != nil {
		t.Fatal(err)
	}

	if version, _ := invokeGreeting(t, c, "worker:live", nil); version != "2" {
		t.Fatalf("unrouted alias ran %s", version)
	}

	for _, tc := range []struct{ name, version, code string }{
		{"live", "1", "ResourceConflictException"},
		{"next", "9", "ResourceNotFoundException"},
		{"7", "1", "InvalidParameterValueException"},
	} {
		_, err = c.CreateAlias(ctx, &sdklambda.CreateAliasInput{
			FunctionName:    aws.String("worker"),
			Name:            aws.String(tc.name),
			FunctionVersion: aws.String(tc.version),
		})
		if got, _ := lambdaCode(err); got != tc.code {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}

	if _, err = c.CreateAlias(ctx, &sdklambda.CreateAliasInput{
		FunctionName:    aws.String("worker"),
		Name:            aws.String("dev"),
		FunctionVersion: aws.String("$LATEST"),
	}); err != nil {
		t.Fatal(err)
	}

	aliases, err := c.ListAliases(ctx, &sdklambda.ListAliasesInput{FunctionName: aws.String("worker")})
	if err != nil || len(aliases.Aliases) != 2 || aws.ToString(aliases.Aliases[0].Name) != "dev" {
		t.Fatalf("%+v %v", aliases, err)
	}

	aliases, err = c.ListAliases(ctx, &sdklambda.ListAliasesInput{
		FunctionName:    aws.String("worker"),
		FunctionVersion: aws.String("2"),
	})
	if err != nil || len(aliases.Aliases) != 1 || aws.ToString(aliases.Aliases[0].Name) != "live" {
		t.Fatalf("%+v %v", aliases, err)
	}

	var versions []string
	pages := sdklambda.NewListVersionsByFunctionPaginator(c, &sdklambda.ListVersionsByFunctionInput{
		FunctionName: aws.String("worker"),
		MaxItems:     aws.Int32(1),
	})
	for pages.HasMorePages() {
		page, pageErr := pages.NextPage(ctx)
		if pageErr != nil || len(page.Versions) != 1 {
			t.Fatalf("%+v %v", page, pageErr)
		}

		versions = append(versions, aws.ToString(page.Versions[0].Version))
	}

	if !slices.Equal(versions, []string{"$LATEST", "1", "2"}) {
		t.Fatal(versions)
	}

	// An event source mapping can invoke an alias.
	q := sqsClient(s, "test")
	queueURL, queue := queueARN(t, q, "jobs")
	mapping, err := c.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		EventSourceArn: &queue,
		FunctionName:   aws.String("worker:live"),
	})
	if err != nil || aws.ToString(mapping.FunctionArn) != arn+":live" {
		t.Fatalf("%+v %v", mapping, err)
	}

	for len(runtime.calls) > 0 {
		<-runtime.calls
	}

	if _, err = q.SendMessage(
		ctx,
		&sqs.SendMessageInput{QueueUrl: &queueURL, MessageBody: aws.String("x")},
	); err != nil {
		t.Fatal(err)
	}

	select {
	case call := <-runtime.calls:
		if call.fn.ARN != arn+":2" || call.fn.Environment["GREETING"] != "v2" {
			t.Fatalf("%+v", call.fn)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the mapping did not invoke the alias")
	}

	if _, err = c.DeleteEventSourceMapping(
		ctx,
		&sdklambda.DeleteEventSourceMappingInput{UUID: mapping.UUID},
	); err != nil {
		t.Fatal(err)
	}

	// A version an alias points to cannot be deleted.
	_, err = c.DeleteFunction(
		ctx,
		&sdklambda.DeleteFunctionInput{FunctionName: aws.String("worker"), Qualifier: aws.String("2")},
	)
	if got, _ := lambdaCode(err); got != "ResourceConflictException" {
		t.Fatalf("delete aliased version: %v", err)
	}

	if _, err = c.DeleteFunction(
		ctx,
		&sdklambda.DeleteFunctionInput{FunctionName: aws.String("worker:1")},
	); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetFunction(ctx, &sdklambda.GetFunctionInput{FunctionName: aws.String("worker:1")})
	if got, _ := lambdaCode(err); got != "ResourceNotFoundException" {
		t.Fatalf("deleted version: %v", err)
	}

	if _, err = c.DeleteAlias(
		ctx,
		&sdklambda.DeleteAliasInput{FunctionName: aws.String("worker"), Name: aws.String("dev")},
	); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetAlias(ctx, &sdklambda.GetAliasInput{FunctionName: aws.String("worker"), Name: aws.String("dev")})
	if got, _ := lambdaCode(err); got != "ResourceNotFoundException" {
		t.Fatalf("deleted alias: %v", err)
	}

	// Deleting the function removes its versions and aliases and releases
	// their environments.
	if _, err = c.DeleteFunction(ctx, &sdklambda.DeleteFunctionInput{FunctionName: aws.String("worker")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetAlias(ctx, &sdklambda.GetAliasInput{FunctionName: aws.String("worker"), Name: aws.String("live")})
	if got, _ := lambdaCode(err); got != "ResourceNotFoundException" {
		t.Fatalf("alias of deleted function: %v", err)
	}

	runtime.mu.Lock()
	released := strings.Join(runtime.released, " ")
	runtime.mu.Unlock()
	if !strings.Contains(released, arn+":1") || !strings.Contains(released, arn+":2") {
		t.Fatal(released)
	}
}
