package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

func TestTutorialSafeRecords(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	secrets := secretsClient(s, "test", "us-east-1")
	_, err := secrets.CreateSecret(
		ctx,
		&secretsmanager.CreateSecretInput{Name: new("tutorial-secret"), SecretString: new("private-secret-value")},
	)
	if err != nil {
		t.Fatal(err)
	}
	secretValue(t, secrets, "tutorial-secret", "")
	parameters := ssmClient(s, "test", "us-east-1")
	put(t, parameters, "tutorial-parameter", "private-parameter-value")
	_, err = parameters.GetParameter(ctx, &ssm.GetParameterInput{Name: new("tutorial-parameter:1")})
	if err != nil {
		t.Fatal(err)
	}
	functions := lambdaClient(s, "test")
	createFunction(t, functions, "tutorial-function", "echo")
	_, err = functions.Invoke(
		ctx,
		&sdklambda.InvokeInput{
			FunctionName: new("tutorial-function"),
			Payload:      []byte(`{"private":"function-payload"}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	states := sfnClient(s)
	machine := createMachine(
		t,
		states,
		"tutorial-machine",
		`{"StartAt":"Hello","States":{"Hello":{"Type":"Pass","End":true}}}`,
		sfntypes.StateMachineTypeStandard,
	)
	started, err := states.StartExecution(
		ctx,
		&sfn.StartExecutionInput{
			StateMachineArn: machine,
			Name:            new("tutorial-execution"),
			Input:           new(`{"private":"workflow-input"}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	finished := finish(t, states, started.ExecutionArn)
	if finished.Status != sfntypes.ExecutionStatusSucceeded {
		t.Fatal(finished.Status)
	}
	_, records := inspect(t, s, http.MethodGet, "/_tinystack/requests")
	expected := map[string]string{
		"GetSecretValue": "tutorial-secret",
		"GetParameter":   "tutorial-parameter:1",
		"Invoke":         "tutorial-function",
	}
	for _, rec := range records {
		target, ok := expected[rec.Operation]
		if !ok {
			continue
		}
		if rec.ResourceIdentifier != target || rec.Status != http.StatusOK || rec.ErrorCode != "" {
			t.Fatalf("unexpected record: %+v", rec)
		}
		if rec.RequestBody != "(redacted: this operation carries sensitive values)" ||
			rec.ResponseBody != rec.RequestBody {
			t.Fatalf("bodies not redacted: %+v", rec)
		}
		delete(expected, rec.Operation)
	}
	if len(expected) != 0 {
		t.Fatal("missing requests", expected)
	}
	var inventory struct {
		Resources []catalog.Owned `json:"resources"`
	}
	if internal(t, s, http.MethodGet, "/_tinystack/resources", &inventory) != http.StatusOK {
		t.Fatal("inventory failed")
	}
	found := false
	for _, r := range inventory.Resources {
		if r.Service != "sfn" || r.Name != "tutorial-machine" {
			continue
		}
		raw, marshalErr := json.Marshal(r.Details)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		var details struct {
			Executions []struct{ Name, Status string }
		}
		if err := json.Unmarshal(raw, &details); err != nil {
			t.Fatal(err)
		}
		if len(details.Executions) != 1 || details.Executions[0].Name != "tutorial-execution" ||
			details.Executions[0].Status != "SUCCEEDED" {
			t.Fatal(string(raw))
		}
		for _, private := range []string{"workflow-input", "Input", "Output", "Payload"} {
			if strings.Contains(string(raw), private) {
				t.Fatalf("inventory exposed %s", private)
			}
		}
		if r.Account != "000000000000" || r.Region != "us-east-1" {
			t.Fatal(r)
		}
		found = true
	}
	if !found {
		t.Fatal("missing machine")
	}
	// Confirm a different scope cannot satisfy the tutorial's resource identity.
	if aws.ToString(finished.StateMachineArn) != *machine {
		t.Fatal(finished.StateMachineArn)
	}
}
