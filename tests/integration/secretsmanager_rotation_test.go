package integration_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/amjadjibon/tinystack/internal/services/lambda"
)

// rotationRuntime runs a rotation function the way the AWS templates do: it
// calls Secrets Manager back through the SDK, adds the AWSPENDING version in
// createSecret, and moves AWSCURRENT to it in finishSecret. The step named
// fail fails.
type rotationRuntime struct {
	server *httptest.Server
	fail   string
	steps  []string
	mu     sync.Mutex
}

func (r *rotationRuntime) Invoke(ctx context.Context, _ *lambda.Function, payload []byte) (lambda.Result, error) {
	var event struct{ Step, SecretId, ClientRequestToken string }
	if err := json.Unmarshal(payload, &event); err != nil {
		return lambda.Result{}, err
	}

	r.mu.Lock()
	r.steps = append(r.steps, event.Step)
	fail := r.fail == event.Step
	r.mu.Unlock()
	if fail {
		return lambda.Result{Payload: []byte(`{"errorMessage":"step failed"}`), Error: true}, nil
	}

	c := secretsClient(r.server, "test", "us-east-1")
	id, token := aws.String(event.SecretId), aws.String(event.ClientRequestToken)
	switch event.Step {
	case "createSecret":
		current, err := c.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: id})
		if err != nil {
			return lambda.Result{}, err
		}

		_, err = c.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
			SecretId: id, ClientRequestToken: token, VersionStages: []string{"AWSPENDING"},
			SecretString: aws.String(aws.ToString(current.SecretString) + "-rotated"),
		})
		if err != nil {
			return lambda.Result{}, err
		}
	case "finishSecret":
		described, err := c.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: id})
		if err != nil {
			return lambda.Result{}, err
		}

		for version, stages := range described.VersionIdsToStages {
			if slices.Contains(stages, "AWSCURRENT") && version != *token {
				_, err = c.UpdateSecretVersionStage(ctx, &secretsmanager.UpdateSecretVersionStageInput{
					SecretId: id, VersionStage: aws.String("AWSCURRENT"), MoveToVersionId: token,
					RemoveFromVersionId: aws.String(version),
				})
				if err != nil {
					return lambda.Result{}, err
				}
			}
		}
	}

	return lambda.Result{Payload: []byte("null")}, nil
}

func (*rotationRuntime) Release(string) {}

func (*rotationRuntime) Close() {}

// ran returns the steps run so far.
func (r *rotationRuntime) ran() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.steps)
}

// waitFor polls until done reports true.
func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	for range 100 {
		if done() {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

func TestSecretsRotation(t *testing.T) {
	runtime := &rotationRuntime{}
	s := serverWith(t, runtime)
	runtime.server = s
	c := secretsClient(s, "test", "us-east-1")
	ctx := context.Background()
	function := aws.ToString(createFunction(t, lambdaClient(s, "test"), "rotator", "app.rotate").FunctionArn)
	_, err := c.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name: aws.String("db"), SecretString: aws.String("one"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.RotateSecret(ctx, &secretsmanager.RotateSecretInput{SecretId: aws.String("db")})
	code(t, err, "InvalidRequestException")

	for _, rules := range []*smtypes.RotationRulesType{
		{AutomaticallyAfterDays: aws.Int64(30), ScheduleExpression: aws.String("rate(10 days)")},
		{ScheduleExpression: aws.String("rate(1 hour)")},
		{ScheduleExpression: aws.String("cron(0 0 L * ? *)")},
		{AutomaticallyAfterDays: aws.Int64(0)},
		{AutomaticallyAfterDays: aws.Int64(30), Duration: aws.String("25h")},
	} {
		_, err = c.RotateSecret(ctx, &secretsmanager.RotateSecretInput{
			SecretId: aws.String("db"), RotationLambdaARN: aws.String(function), RotationRules: rules,
		})
		code(t, err, "InvalidParameterException")
	}

	rotated, err := c.RotateSecret(ctx, &secretsmanager.RotateSecretInput{
		SecretId: aws.String("db"), RotationLambdaARN: aws.String(function),
		RotationRules: &smtypes.RotationRulesType{AutomaticallyAfterDays: aws.Int64(30), Duration: aws.String("2h")},
	})
	if err != nil || aws.ToString(rotated.VersionId) == "" {
		t.Fatalf("got %+v, %v", rotated, err)
	}

	describe := func() *secretsmanager.DescribeSecretOutput {
		t.Helper()
		out, describeErr := c.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String("db")})
		if describeErr != nil {
			t.Fatal(describeErr)
		}

		return out
	}

	waitFor(t, "the rotation", func() bool { return describe().LastRotatedDate != nil })
	if steps := runtime.ran(); !slices.Equal(
		steps,
		[]string{"createSecret", "setSecret", "testSecret", "finishSecret"},
	) {
		t.Fatalf("steps %v", steps)
	}

	if got := secretValue(t, c, "db", ""); got != "one-rotated" {
		t.Fatalf("current value %q", got)
	}

	if got := secretValue(t, c, "db", "AWSPREVIOUS"); got != "one" {
		t.Fatalf("previous value %q", got)
	}

	d := describe()
	next := time.Now().Add(30 * 24 * time.Hour)
	if !aws.ToBool(d.RotationEnabled) || aws.ToString(d.RotationLambdaARN) != function ||
		aws.ToInt64(d.RotationRules.AutomaticallyAfterDays) != 30 || aws.ToString(d.RotationRules.Duration) != "2h" ||
		d.NextRotationDate == nil || d.NextRotationDate.Sub(next).Abs() > time.Minute {
		t.Fatalf("got %+v", d)
	}

	listed, err := c.ListSecrets(ctx, &secretsmanager.ListSecretsInput{})
	if err != nil || len(listed.SecretList) != 1 || !aws.ToBool(listed.SecretList[0].RotationEnabled) {
		t.Fatalf("got %+v, %v", listed, err)
	}

	// A failed step leaves the new version AWSPENDING, which blocks another
	// rotation until one retries it.
	runtime.mu.Lock()
	runtime.fail, runtime.steps = "testSecret", nil
	runtime.mu.Unlock()
	failed, err := c.RotateSecret(ctx, &secretsmanager.RotateSecretInput{SecretId: aws.String("db")})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the failed step", func() bool { return slices.Contains(runtime.ran(), "testSecret") })
	time.Sleep(50 * time.Millisecond)
	if got := secretValue(t, c, "db", ""); got != "one-rotated" {
		t.Fatalf("current value %q", got)
	}

	if got := secretValue(t, c, "db", "AWSPENDING"); got != "one-rotated-rotated" {
		t.Fatalf("pending value %q", got)
	}

	_, err = c.RotateSecret(ctx, &secretsmanager.RotateSecretInput{SecretId: aws.String("db")})
	code(t, err, "InvalidRequestException")

	runtime.mu.Lock()
	runtime.fail = ""
	runtime.mu.Unlock()
	_, err = c.RotateSecret(ctx, &secretsmanager.RotateSecretInput{
		SecretId: aws.String("db"), ClientRequestToken: failed.VersionId,
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the retried rotation", func() bool { return secretValue(t, c, "db", "") == "one-rotated-rotated" })

	// Cancelling turns rotation off but keeps the configuration.
	cancelled, err := c.CancelRotateSecret(ctx, &secretsmanager.CancelRotateSecretInput{SecretId: aws.String("db")})
	if err != nil || aws.ToString(cancelled.Name) != "db" {
		t.Fatalf("got %+v, %v", cancelled, err)
	}

	if d = describe(); aws.ToBool(d.RotationEnabled) || d.NextRotationDate != nil ||
		aws.ToString(d.RotationLambdaARN) != function {
		t.Fatalf("got %+v", d)
	}

	// A missing function fails the rotation's first step, which the delivery
	// log records; the secret is unchanged.
	_, err = c.RotateSecret(ctx, &secretsmanager.RotateSecretInput{
		SecretId:          aws.String("db"),
		RotationLambdaARN: aws.String("arn:aws:lambda:us-east-1:000000000000:function:missing"),
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the failed delivery", func() bool {
		for _, d := range deliveries(t, s, 0) {
			if d.Via == "secretsmanager" && d.Error != "" {
				return true
			}
		}

		return false
	})

	if got := secretValue(t, c, "db", ""); got != "one-rotated-rotated" {
		t.Fatalf("current value %q", got)
	}
}
