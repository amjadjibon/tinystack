package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"

	"github.com/amjadjibon/tinystack/internal/services/lambda"
)

func TestLambdaReservedConcurrency(t *testing.T) {
	c := lambdaClient(server(t), "test")
	ctx := context.Background()
	createFunction(t, c, "capped", "app.echo")
	got, err := c.GetFunctionConcurrency(ctx, &sdklambda.GetFunctionConcurrencyInput{
		FunctionName: aws.String("capped"),
	})
	if err != nil || got.ReservedConcurrentExecutions != nil {
		t.Fatalf("none: %+v %v", got, err)
	}

	// Reserving no executions turns the function off.
	if _, err = c.PutFunctionConcurrency(ctx, &sdklambda.PutFunctionConcurrencyInput{
		FunctionName: aws.String("capped"), ReservedConcurrentExecutions: aws.Int32(0),
	}); err != nil {
		t.Fatal(err)
	}

	var throttled *lambdatypes.TooManyRequestsException
	_, err = c.Invoke(ctx, &sdklambda.InvokeInput{FunctionName: aws.String("capped")})
	if !errors.As(err, &throttled) {
		t.Fatalf("invoked with no executions reserved: %v", err)
	}

	function, err := c.GetFunction(ctx, &sdklambda.GetFunctionInput{FunctionName: aws.String("capped")})
	if err != nil || function.Concurrency == nil ||
		aws.ToInt32(function.Concurrency.ReservedConcurrentExecutions) != 0 {
		t.Fatalf("GetFunction: %+v %v", function, err)
	}

	var invalid *lambdatypes.InvalidParameterValueException
	if _, err = c.PutFunctionConcurrency(ctx, &sdklambda.PutFunctionConcurrencyInput{
		FunctionName: aws.String("capped"), ReservedConcurrentExecutions: aws.Int32(901),
	}); !errors.As(err, &invalid) {
		t.Fatalf("over the account's unreserved minimum: %v", err)
	}

	if _, err = c.DeleteFunctionConcurrency(ctx, &sdklambda.DeleteFunctionConcurrencyInput{
		FunctionName: aws.String("capped"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.Invoke(ctx, &sdklambda.InvokeInput{FunctionName: aws.String("capped")}); err != nil {
		t.Fatalf("after deleting the limit: %v", err)
	}
}

func TestLambdaProvisionedConcurrency(t *testing.T) {
	c := lambdaClient(server(t), "test")
	ctx := context.Background()
	createFunction(t, c, "warm", "app.echo")
	version, err := c.PublishVersion(ctx, &sdklambda.PublishVersionInput{FunctionName: aws.String("warm")})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreateAlias(ctx, &sdklambda.CreateAliasInput{
		FunctionName: aws.String("warm"), Name: aws.String("live"), FunctionVersion: version.Version,
	}); err != nil {
		t.Fatal(err)
	}

	for _, qualifier := range []string{"live", aws.ToString(version.Version)} {
		out, putErr := c.PutProvisionedConcurrencyConfig(ctx, &sdklambda.PutProvisionedConcurrencyConfigInput{
			FunctionName: aws.String("warm"), Qualifier: aws.String(qualifier),
			ProvisionedConcurrentExecutions: aws.Int32(2),
		})
		if putErr != nil || out.Status != lambdatypes.ProvisionedConcurrencyStatusEnumInProgress ||
			aws.ToInt32(out.AvailableProvisionedConcurrentExecutions) != 0 {
			t.Fatalf("%s: %+v %v", qualifier, out, putErr)
		}
	}

	var invalid *lambdatypes.InvalidParameterValueException
	if _, err = c.PutProvisionedConcurrencyConfig(ctx, &sdklambda.PutProvisionedConcurrencyConfigInput{
		FunctionName: aws.String("warm"), Qualifier: aws.String("$LATEST"),
		ProvisionedConcurrentExecutions: aws.Int32(1),
	}); !errors.As(err, &invalid) {
		t.Fatalf("$LATEST: %v", err)
	}

	list, err := c.ListProvisionedConcurrencyConfigs(ctx, &sdklambda.ListProvisionedConcurrencyConfigsInput{
		FunctionName: aws.String("warm"),
	})
	if err != nil || len(list.ProvisionedConcurrencyConfigs) != 2 ||
		aws.ToString(list.ProvisionedConcurrencyConfigs[0].FunctionArn) !=
			"arn:aws:lambda:us-east-1:000000000000:function:warm:1" {
		t.Fatalf("list: %+v %v", list, err)
	}

	// Deleting the alias deletes its configuration.
	if _, err = c.DeleteAlias(ctx, &sdklambda.DeleteAliasInput{
		FunctionName: aws.String("warm"), Name: aws.String("live"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteProvisionedConcurrencyConfig(ctx, &sdklambda.DeleteProvisionedConcurrencyConfigInput{
		FunctionName: aws.String("warm"), Qualifier: version.Version,
	}); err != nil {
		t.Fatal(err)
	}

	var missing *lambdatypes.ProvisionedConcurrencyConfigNotFoundException
	if _, err = c.GetProvisionedConcurrencyConfig(ctx, &sdklambda.GetProvisionedConcurrencyConfigInput{
		FunctionName: aws.String("warm"), Qualifier: version.Version,
	}); !errors.As(err, &missing) {
		t.Fatalf("deleted: %v", err)
	}

	list, err = c.ListProvisionedConcurrencyConfigs(ctx, &sdklambda.ListProvisionedConcurrencyConfigsInput{
		FunctionName: aws.String("warm"),
	})
	if err != nil || len(list.ProvisionedConcurrencyConfigs) != 0 {
		t.Fatalf("left: %+v %v", list, err)
	}
}

type warmedLambda struct {
	warmed chan warmAllocation
	fakeRuntime
}

type warmAllocation struct {
	version string
	pool    string
	count   int
}

func (r *warmedLambda) Warm(_ context.Context, fn *lambda.Function, count int) error {
	r.warmed <- warmAllocation{fn.Version, fn.Pool(), count}
	return nil
}

func TestLambdaWeightedProvisionedConcurrency(t *testing.T) {
	ctx := context.Background()
	runtime := &warmedLambda{
		fakeRuntime: fakeRuntime{calls: make(chan invocation, 2)},
		warmed:      make(chan warmAllocation, 4),
	}
	s := serverWith(t, runtime)
	c := lambdaClient(s, "test")
	createFunction(t, c, "weighted", "app.echo")
	one, err := c.PublishVersion(ctx, &sdklambda.PublishVersionInput{FunctionName: new("weighted")})
	mustAuthorize(t, err)
	_, err = c.UpdateFunctionConfiguration(ctx, &sdklambda.UpdateFunctionConfigurationInput{
		FunctionName: new("weighted"), Description: new("second version"),
	})
	mustAuthorize(t, err)
	two, err := c.PublishVersion(ctx, &sdklambda.PublishVersionInput{FunctionName: new("weighted")})
	mustAuthorize(t, err)
	_, err = c.CreateAlias(ctx, &sdklambda.CreateAliasInput{
		FunctionName: new("weighted"), Name: new("live"), FunctionVersion: one.Version,
		RoutingConfig: &lambdatypes.AliasRoutingConfiguration{AdditionalVersionWeights: map[string]float64{
			*two.Version: 0.25,
		}},
	})
	mustAuthorize(t, err)
	_, err = c.PutProvisionedConcurrencyConfig(ctx, &sdklambda.PutProvisionedConcurrencyConfigInput{
		FunctionName: new("weighted"), Qualifier: new("live"), ProvisionedConcurrentExecutions: new(int32(4)),
	})
	mustAuthorize(t, err)
	ready := waitLambdaProvisioned(t, c, "weighted", "live", lambdatypes.ProvisionedConcurrencyStatusEnumReady)
	if *ready.AllocatedProvisionedConcurrentExecutions != 4 || len(runtime.warmed) != 2 {
		t.Fatalf("provisioned capacity = %+v, pools=%d", ready, len(runtime.warmed))
	}

	first, second := <-runtime.warmed, <-runtime.warmed
	if first.version != *one.Version || first.count != 3 || second.version != *two.Version ||
		second.count != 1 || first.pool == second.pool {
		t.Fatalf("allocations = %+v %+v", first, second)
	}

	_, err = c.DeleteFunction(
		ctx,
		&sdklambda.DeleteFunctionInput{FunctionName: new("weighted"), Qualifier: two.Version},
	)
	if !isType[*lambdatypes.ResourceConflictException](err) {
		t.Fatalf("delete weighted target: %v", err)
	}

	_, err = c.UpdateAlias(ctx, &sdklambda.UpdateAliasInput{
		FunctionName: new("weighted"), Name: new("live"),
		RoutingConfig: &lambdatypes.AliasRoutingConfiguration{AdditionalVersionWeights: map[string]float64{
			*two.Version: 1,
		}},
	})
	mustAuthorize(t, err)
	waitLambdaProvisioned(t, c, "weighted", "live", lambdatypes.ProvisionedConcurrencyStatusEnumReady)
	if len(runtime.warmed) != 1 {
		t.Fatalf("reallocated pools = %d", len(runtime.warmed))
	}

	shifted := <-runtime.warmed
	if shifted.version != *two.Version || shifted.count != 4 {
		t.Fatalf("allocation after routing update = %+v", shifted)
	}

	invoked, err := c.Invoke(ctx, &sdklambda.InvokeInput{
		FunctionName: new("weighted"), Qualifier: new("live"), Payload: []byte(`{}`),
	})
	mustAuthorize(t, err)
	call := <-runtime.calls
	if *invoked.ExecutedVersion != *two.Version || call.fn.Pool() != shifted.pool {
		t.Fatalf("invoked %v in pool %s, want %s", invoked.ExecutedVersion, call.fn.Pool(), shifted.pool)
	}

	m := cloudwatchClient(s)
	dims := dimensions("FunctionName", "weighted", "Resource", "weighted:live")
	expectBuiltin(t, m, "AWS/Lambda", "ProvisionedConcurrentExecutions", "Count", dims, 1)
	expectBuiltin(t, m, "AWS/Lambda", "ProvisionedConcurrencyUtilization", "None", dims, 0.25)
}

func TestLambdaGetAccountSettings(t *testing.T) {
	s := server(t)
	c := lambdaClient(s, "test")
	ctx := context.Background()
	createFunction(t, c, "measured", "app.echo")
	version, err := c.PublishVersion(ctx, &sdklambda.PublishVersionInput{FunctionName: new("measured")})
	mustAuthorize(t, err)
	_, err = c.PutFunctionConcurrency(ctx, &sdklambda.PutFunctionConcurrencyInput{
		FunctionName: new("measured"), ReservedConcurrentExecutions: new(int32(20)),
	})
	mustAuthorize(t, err)
	out, err := c.GetAccountSettings(ctx, &sdklambda.GetAccountSettingsInput{})
	mustAuthorize(t, err)
	if out.AccountLimit == nil || out.AccountLimit.ConcurrentExecutions != 1000 ||
		*out.AccountLimit.UnreservedConcurrentExecutions != 980 || out.AccountUsage == nil ||
		out.AccountUsage.FunctionCount != 1 || out.AccountUsage.TotalCodeSize != 2*version.CodeSize {
		t.Fatalf("account settings = %+v", out)
	}
}
