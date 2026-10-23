package integration_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"

	"github.com/amjadjibon/tinystack/internal/services/lambda/local"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func waitLambdaProvisioned(t *testing.T, c *sdklambda.Client, name, qualifier string,
	status types.ProvisionedConcurrencyStatusEnum,
) *sdklambda.GetProvisionedConcurrencyConfigOutput {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := c.GetProvisionedConcurrencyConfig(
			context.Background(),
			&sdklambda.GetProvisionedConcurrencyConfigInput{
				FunctionName: new(name), Qualifier: new(qualifier),
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		if got.Status == status {
			return got
		}

		if got.Status == types.ProvisionedConcurrencyStatusEnumFailed || time.Now().After(deadline) {
			t.Fatalf("provisioned status: %+v", got)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func TestLambdaLocalProvisionedLifecycle(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is absent")
	}

	for _, action := range []string{"config", "alias", "version", "function", "replace", "retarget", "reset", "stop"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			runtime := local.New(local.Options{})
			s, stop := serverStore(t, testConfig(), runtime, &memory.Store{})
			t.Cleanup(stop)
			c := lambdaClient(s, "test")
			marker := filepath.Join(t.TempDir(), "started")
			code := fmt.Sprintf(`import os
with open(%s, 'a') as f: f.write(os.environ['AWS_LAMBDA_RUNTIME_API']+'\n')
def handler(e,c): return os.environ['AWS_LAMBDA_RUNTIME_API']
`, strconv.Quote(marker))
			created, err := c.CreateFunction(ctx, &sdklambda.CreateFunctionInput{
				FunctionName: new("prestarted"), Runtime: types.RuntimePython312, Handler: new("app.handler"),
				Role: new("arn:aws:iam::000000000000:role/lambda"), Publish: true,
				Code: &types.FunctionCode{ZipFile: zipOf(t, "app.py", code)},
			})
			if err != nil {
				t.Fatal(err)
			}

			qualifier := *created.Version
			if action == "alias" || action == "retarget" {
				_, err = c.CreateAlias(ctx, &sdklambda.CreateAliasInput{
					FunctionName: new("prestarted"), Name: new("live"), FunctionVersion: created.Version,
				})
				if err != nil {
					t.Fatal(err)
				}

				qualifier = "live"
			}

			put, err := c.PutProvisionedConcurrencyConfig(ctx, &sdklambda.PutProvisionedConcurrencyConfigInput{
				FunctionName: new(
					"prestarted",
				),
				Qualifier:                       new(qualifier),
				ProvisionedConcurrentExecutions: new(int32(2)),
			})
			if err != nil || put.Status != types.ProvisionedConcurrencyStatusEnumInProgress {
				t.Fatalf("put: %+v %v", put, err)
			}

			ready := waitLambdaProvisioned(t, c, "prestarted", qualifier, types.ProvisionedConcurrencyStatusEnumReady)
			if *ready.AllocatedProvisionedConcurrentExecutions != 2 {
				t.Fatalf("allocated: %+v", ready)
			}

			// Initialization has completed in two different processes before any invocation.
			data, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}

			endpoints := strings.Fields(string(data))
			if len(endpoints) != 2 || endpoints[0] == endpoints[1] {
				t.Fatalf("environments: %s", data)
			}

			invoked, err := c.Invoke(ctx, &sdklambda.InvokeInput{
				FunctionName: new("prestarted"), Qualifier: new(qualifier), Payload: []byte(`{}`),
			})
			if err != nil {
				t.Fatal(err)
			}

			reused := strings.Trim(string(invoked.Payload), `"`)
			if reused != endpoints[0] && reused != endpoints[1] {
				t.Fatalf("not reused: %s", invoked.Payload)
			}

			switch action {
			case "config":
				_, err = c.DeleteProvisionedConcurrencyConfig(ctx, &sdklambda.DeleteProvisionedConcurrencyConfigInput{
					FunctionName: new("prestarted"), Qualifier: new(qualifier),
				})
			case "alias":
				_, err = c.DeleteAlias(
					ctx,
					&sdklambda.DeleteAliasInput{FunctionName: new("prestarted"), Name: new("live")},
				)
			case "version":
				_, err = c.DeleteFunction(ctx, &sdklambda.DeleteFunctionInput{
					FunctionName: new("prestarted"), Qualifier: created.Version,
				})
			case "function":
				_, err = c.DeleteFunction(ctx, &sdklambda.DeleteFunctionInput{FunctionName: new("prestarted")})
			case "replace":
				_, err = c.PutProvisionedConcurrencyConfig(ctx, &sdklambda.PutProvisionedConcurrencyConfigInput{
					FunctionName: new(
						"prestarted",
					),
					Qualifier:                       new(qualifier),
					ProvisionedConcurrentExecutions: new(int32(1)),
				})
			case "retarget":
				_, err = c.UpdateFunctionConfiguration(ctx, &sdklambda.UpdateFunctionConfigurationInput{
					FunctionName: new("prestarted"), Description: new("new revision"),
				})
				if err != nil {
					t.Fatal(err)
				}

				var version *sdklambda.PublishVersionOutput
				version, err = c.PublishVersion(ctx, &sdklambda.PublishVersionInput{FunctionName: new("prestarted")})
				if err != nil {
					t.Fatal(err)
				}

				_, err = c.UpdateAlias(ctx, &sdklambda.UpdateAliasInput{
					FunctionName: new("prestarted"), Name: new("live"), FunctionVersion: version.Version,
				})
			case "reset":
				var response *http.Response
				response, err = http.Post(s.URL+"/_tinystack/reset", "application/json", http.NoBody)
				if err == nil {
					_ = response.Body.Close()
				}
			case "stop":
				stop()
			}
			if err != nil {
				t.Fatal(err)
			}
			if action == "replace" || action == "retarget" {
				waitLambdaProvisioned(t, c, "prestarted", qualifier, types.ProvisionedConcurrencyStatusEnumReady)
			}

			for _, endpoint := range endpoints {
				address, _, _ := strings.Cut(endpoint, "/")
				conn, dialErr := net.DialTimeout("tcp", address, 100*time.Millisecond)
				if dialErr == nil {
					_ = conn.Close()
					t.Fatalf("environment still listening: %s", address)
				}
			}
		})
	}
}

func TestLambdaLocalProvisionedFailure(t *testing.T) {
	c := lambdaClient(serverWith(t, local.New(local.Options{Python: "tinystack-missing-interpreter"})), "test")
	createFunction(t, c, "init-failure", "app.echo")
	ctx := context.Background()
	version, err := c.PublishVersion(ctx, &sdklambda.PublishVersionInput{FunctionName: new("init-failure")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.PutProvisionedConcurrencyConfig(ctx, &sdklambda.PutProvisionedConcurrencyConfigInput{
		FunctionName: new("init-failure"), Qualifier: version.Version, ProvisionedConcurrentExecutions: new(int32(1)),
	})
	if err != nil {
		t.Fatal(err)
	}

	failed := waitLambdaProvisioned(
		t,
		c,
		"init-failure",
		*version.Version,
		types.ProvisionedConcurrencyStatusEnumFailed,
	)
	if failed.StatusReason == nil || !strings.Contains(*failed.StatusReason, "unavailable") ||
		*failed.AllocatedProvisionedConcurrentExecutions != 0 {
		t.Fatalf("failed: %+v", failed)
	}
}
