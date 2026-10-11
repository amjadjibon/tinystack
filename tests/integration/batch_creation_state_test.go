package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	sdk "github.com/aws/aws-sdk-go-v2/service/batch"
	"github.com/aws/aws-sdk-go-v2/service/batch/types"
)

func batchCreationEnvironments(t *testing.T, ctx context.Context, c *sdk.Client) []types.ComputeEnvironmentDetail {
	t.Helper()
	out, err := c.DescribeComputeEnvironments(ctx, &sdk.DescribeComputeEnvironmentsInput{})
	if err != nil {
		t.Fatal(err)
	}
	return out.ComputeEnvironments
}

func TestBatchSDKComputeEnvironmentCreationState(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := batchClient(s, "test", "us-east-1")
	batchFixture(t, s)
	baseline := batchCreationEnvironments(t, ctx, c)
	resources := baseline[0].ComputeResources
	for _, typ := range []types.CEType{types.CETypeManaged, types.CETypeUnmanaged} {
		for _, state := range []types.CEState{types.CEStateDisabled, "", types.CEStateEnabled} {
			t.Run(string(typ)+"/"+string(state), func(t *testing.T) {
				before := batchCreationEnvironments(t, ctx, c)
				in := &sdk.CreateComputeEnvironmentInput{
					ComputeEnvironmentName: new("sdk-" + string(typ) + "-" + string(state)),
					Type:                   typ,
					State:                  state,
					ServiceRole:            new("batch"),
				}
				if typ == types.CETypeManaged {
					in.ComputeResources = resources
				}
				created, err := c.CreateComputeEnvironment(ctx, in)
				if state == types.CEStateDisabled {
					var clientErr *types.ClientException
					var responseErr *awshttp.ResponseError
					if !errors.As(err, &clientErr) || !errors.As(err, &responseErr) ||
						responseErr.HTTPStatusCode() != http.StatusBadRequest {
						t.Fatalf("disabled create: want ClientException/400, got %v", err)
					}
					if after := batchCreationEnvironments(t, ctx, c); !reflect.DeepEqual(before, after) {
						t.Fatalf("rejected create changed metadata: before=%#v after=%#v", before, after)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				query := &sdk.DescribeComputeEnvironmentsInput{
					ComputeEnvironments: []string{*created.ComputeEnvironmentArn},
				}
				out, err := c.DescribeComputeEnvironments(ctx, query)
				if err != nil || len(out.ComputeEnvironments) != 1 ||
					out.ComputeEnvironments[0].State != types.CEStateEnabled {
					t.Fatalf("enabled creation: %v %v", out, err)
				}
				if _, err = c.UpdateComputeEnvironment(ctx, &sdk.UpdateComputeEnvironmentInput{
					ComputeEnvironment: created.ComputeEnvironmentArn,
					State:              types.CEStateDisabled,
				}); err != nil {
					t.Fatal(err)
				}
				out, err = c.DescribeComputeEnvironments(ctx, query)
				if err != nil || len(out.ComputeEnvironments) != 1 ||
					out.ComputeEnvironments[0].State != types.CEStateDisabled ||
					*out.ComputeEnvironments[0].ComputeEnvironmentArn != *created.ComputeEnvironmentArn {
					t.Fatalf("disable update: %v %v", out, err)
				}
			})
		}
	}
}

func TestBatchRawWireComputeEnvironmentCreationState(t *testing.T) {
	s := server(t)
	c := batchClient(s, "test", "us-east-1")
	batchFixture(t, s)
	status, out := batchWire(t, s, "POST", "/v1/describecomputeenvironments", `{}`)
	if status != http.StatusOK {
		t.Fatalf("describe fixture: %d %v", status, out)
	}
	resources := out["computeEnvironments"].([]any)[0].(map[string]any)["computeResources"]
	for _, typ := range []string{"MANAGED", "UNMANAGED"} {
		for _, state := range []string{"DISABLED", "", "ENABLED"} {
			t.Run(typ+"/"+state, func(t *testing.T) {
				before := batchCreationEnvironments(t, context.Background(), c)
				body := map[string]any{
					"computeEnvironmentName": "wire-" + typ + "-" + state,
					"type":                   typ,
					"serviceRole":            "batch",
				}
				if state != "" {
					body["state"] = state
				}
				if typ == "MANAGED" {
					body["computeResources"] = resources
				}
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				status, created := batchWire(t, s, "POST", "/v1/createcomputeenvironment", string(raw))
				if state == "DISABLED" {
					if status != http.StatusBadRequest || created["__type"] != "ClientException" ||
						created["message"] == nil {
						t.Fatalf("disabled create: want ClientException/400, got %d %v", status, created)
					}
					if after := batchCreationEnvironments(
						t,
						context.Background(),
						c,
					); !reflect.DeepEqual(
						before,
						after,
					) {
						t.Fatalf("rejected create changed metadata: before=%#v after=%#v", before, after)
					}
					return
				}
				if status != http.StatusOK || created["computeEnvironmentArn"] == nil {
					t.Fatalf("enabled creation: %d %v", status, created)
				}
				query, err := json.Marshal(
					map[string]any{"computeEnvironments": []any{created["computeEnvironmentArn"]}},
				)
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{"ENABLED", "DISABLED"} {
					if want == "DISABLED" {
						update, err := json.Marshal(map[string]any{
							"computeEnvironment": created["computeEnvironmentArn"], "state": want,
						})
						if err != nil {
							t.Fatal(err)
						}
						if status, out := batchWire(
							t,
							s,
							"POST",
							"/v1/updatecomputeenvironment",
							string(update),
						); status != http.StatusOK {
							t.Fatalf("disable update: %d %v", status, out)
						}
					}
					status, out := batchWire(t, s, "POST", "/v1/describecomputeenvironments", string(query))
					if status != http.StatusOK {
						t.Fatalf("describe: %d %v", status, out)
					}
					envs, _ := out["computeEnvironments"].([]any)
					if len(envs) != 1 {
						t.Fatalf("describe: %v", out)
					}
					env := envs[0].(map[string]any)
					if env["state"] != want || env["computeEnvironmentArn"] != created["computeEnvironmentArn"] {
						t.Fatalf("want %s with original ARN: %v", want, env)
					}
				}
			})
		}
	}
}
