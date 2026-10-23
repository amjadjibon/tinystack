package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
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
