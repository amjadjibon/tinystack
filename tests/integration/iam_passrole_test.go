package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/gateway"
)

const passedRoleARN = "arn:aws:iam::000000000000:role/passed"

const passRolePrimary = `{"Effect":"Allow","Action":["lambda:*","ecs:*","states:*","events:*"],"Resource":"*"}`

func passRoleDocument(statements ...string) string {
	return `{"Statement":[` + strings.Join(statements, ",") + `]}`
}

func passRoleAllow(principal string) string {
	return fmt.Sprintf(`{"Effect":"Allow","Action":"iam:PassRole","Resource":%q,`+
		`"Condition":{"StringEquals":{"iam:PassedToService":%q}}}`, passedRoleARN, principal)
}

type passRoleOperation struct {
	invoke          func(context.Context, *httptest.Server, aws.CredentialsProvider) error
	name, principal string
}

func passRoleOperations(t *testing.T) []passRoleOperation {
	t.Helper()
	return []passRoleOperation{
		{name: "CreateFunction", principal: "lambda.amazonaws.com", invoke: func(
			ctx context.Context, s *httptest.Server, credentials aws.CredentialsProvider,
		) error {
			client := lambda.New(lambda.Options{
				Region: "us-east-1", BaseEndpoint: new(s.URL),
				Credentials: credentials, Retryer: aws.NopRetryer{},
			})
			_, err := client.CreateFunction(ctx, &lambda.CreateFunctionInput{
				FunctionName: new("passed"),
				Role:         new(passedRoleARN),
				Runtime:      lambdatypes.RuntimePython312,
				Handler: new(
					"app.echo",
				),
				Code: &lambdatypes.FunctionCode{ZipFile: zipOf(t, "app.py", "def echo(e,c): return e")},
			})
			return err
		}},
		{name: "RegisterTaskDefinition", principal: "ecs-tasks.amazonaws.com", invoke: func(
			ctx context.Context, s *httptest.Server, credentials aws.CredentialsProvider,
		) error {
			client := ecs.New(ecs.Options{
				Region: "us-east-1", BaseEndpoint: new(s.URL),
				Credentials: credentials, Retryer: aws.NopRetryer{},
			})
			_, err := client.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
				Family:           new("passed"),
				TaskRoleArn:      new(passedRoleARN),
				ExecutionRoleArn: new(passedRoleARN),
				ContainerDefinitions: []ecstypes.ContainerDefinition{
					{Name: new("web"), Image: new("nginx"), Memory: new(int32(128))},
				},
			})
			return err
		}},
		{name: "CreateStateMachine", principal: "states.amazonaws.com", invoke: func(
			ctx context.Context, s *httptest.Server, credentials aws.CredentialsProvider,
		) error {
			client := sfn.New(sfn.Options{
				Region: "us-east-1", BaseEndpoint: new(s.URL),
				Credentials: credentials, Retryer: aws.NopRetryer{},
			})
			_, err := client.CreateStateMachine(ctx, &sfn.CreateStateMachineInput{
				Name: new("passed"), RoleArn: new(passedRoleARN),
				Definition: new(`{"StartAt":"Done","States":{"Done":{"Type":"Succeed"}}}`),
			})
			return err
		}},
		{name: "PutTargets", principal: "events.amazonaws.com", invoke: func(
			ctx context.Context, s *httptest.Server, credentials aws.CredentialsProvider,
		) error {
			_, ruleErr := eventsClient(s, "test").PutRule(ctx, &eventbridge.PutRuleInput{
				Name: new("passed"), EventPattern: new(orderPattern),
			})
			mustAuthorize(t, ruleErr)
			client := eventbridge.New(eventbridge.Options{
				Region: "us-east-1", BaseEndpoint: new(s.URL),
				Credentials: credentials, Retryer: aws.NopRetryer{},
			})
			out, err := client.PutTargets(ctx, &eventbridge.PutTargetsInput{
				Rule: new("passed"), Targets: []ebtypes.Target{
					{Id: new("one"), Arn: new("arn:aws:sqs:us-east-1:000000000000:one"), RoleArn: new(passedRoleARN)},
					{Id: new("two"), Arn: new("arn:aws:sqs:us-east-1:000000000000:two"), RoleArn: new(passedRoleARN)},
				},
			})
			if err == nil && out.FailedEntryCount != 0 {
				t.Fatalf("failed targets: %+v", out)
			}

			return err
		}},
	}
}

func passRoleInspector(t *testing.T, s *httptest.Server, operation, mode, layer string) {
	t.Helper()
	response, err := http.Get(s.URL + "/_tinystack/requests")
	mustAuthorize(t, err)
	defer response.Body.Close()
	var page struct {
		Requests []gateway.Record `json:"requests"`
	}

	mustAuthorize(t, json.NewDecoder(response.Body).Decode(&page))
	for i := range page.Requests {
		record := &page.Requests[i]
		if record.Operation != operation {
			continue
		}

		decision := record.Authorization
		if mode == config.AuthorizationOff {
			if decision != nil {
				t.Fatal("authorization off evaluated PassRole")
			}

			return
		}

		if decision == nil || len(decision.Checks) != 2 {
			t.Fatalf("missing primary/PassRole checks: %+v", decision)
		}

		check := decision.Checks[1]
		if check.Action != "iam:PassRole" || check.Resource != passedRoleARN || check.WildcardOnly ||
			!decision.Checks[0].Allowed || check.Allowed != (layer == "") || decision.Allowed != (layer == "") {
			t.Fatalf("incorrect PassRole decision: %+v", decision)
		}

		if layer != "" && !strings.Contains(decision.Message, layer) {
			t.Fatal(decision.Message)
		}

		return
	}

	t.Fatal("missing inspector record for " + operation)
}

func TestIAMPassRole(t *testing.T) {
	for _, operation := range passRoleOperations(t) {
		t.Run(operation.name, func(t *testing.T) {
			for _, test := range []struct{ name, grant, boundary, layer string }{
				{name: "allow", grant: passRoleAllow(operation.principal)},
				{name: "missing", layer: "identity-based policy"},
				{name: "explicit-deny", grant: passRoleAllow(operation.principal) +
					`,{"Effect":"Deny","Action":"iam:PassRole","Resource":"*"}`, layer: "explicit deny"},
				{name: "wrong-service", grant: passRoleAllow("wrong.amazonaws.com"), layer: "identity-based policy"},
				{
					name: "boundary", grant: passRoleAllow(operation.principal),
					boundary: passRoleDocument(passRolePrimary), layer: "permissions boundary",
				},
				{name: "boundary-allows", grant: passRoleAllow(operation.principal), boundary: boundaryBroad},
			} {
				t.Run(test.name, func(t *testing.T) {
					s := authorizationServer(t, config.AuthorizationEnforce, true)
					key := authorizationUser(t, s)
					statements := []string{passRolePrimary}
					if test.grant != "" {
						statements = append(statements, test.grant)
					}

					authorizationPolicy(t, s, passRoleDocument(statements...))
					if test.boundary != "" {
						root := iamClient(s, "test", "us-east-1")
						_, err := root.PutUserPermissionsBoundary(
							context.Background(),
							&iam.PutUserPermissionsBoundaryInput{
								UserName: new(
									"alice",
								),
								PermissionsBoundary: boundaryManaged(t, root, "pass-cap", test.boundary),
							},
						)
						mustAuthorize(t, err)
					}

					err := operation.invoke(context.Background(), s, authorizationProvider(key))
					if test.layer == "" {
						mustAuthorize(t, err)
					} else {
						wantCode(t, err, "AccessDeniedException")
						if !strings.Contains(err.Error(), "iam:PassRole") ||
							!strings.Contains(err.Error(), passedRoleARN) ||
							!strings.Contains(err.Error(), test.layer) {
							t.Fatal(err)
						}
					}

					passRoleInspector(t, s, operation.name, config.AuthorizationEnforce, test.layer)
				})
			}
		})
	}
}

func TestIAMPassRoleModesAndSessionPolicies(t *testing.T) {
	operation := passRoleOperations(t)[1]
	for _, mode := range []string{config.AuthorizationOff, config.AuthorizationSoft, config.AuthorizationEnforce} {
		t.Run(mode, func(t *testing.T) {
			s := authorizationServer(t, mode, true)
			root := iamClient(s, "test", "us-east-1")
			role := boundaryRole(t, root, nil)
			assumed, err := stsClient(s, "test", "test").AssumeRole(context.Background(), &sts.AssumeRoleInput{
				RoleArn: role, RoleSessionName: new("pass-session"), Policy: new(passRoleDocument(passRolePrimary)),
			})
			mustAuthorize(t, err)
			err = operation.invoke(context.Background(), s, temporary(assumed.Credentials))
			if mode == config.AuthorizationEnforce {
				wantCode(t, err, "AccessDeniedException")
				if !strings.Contains(err.Error(), "session policy") {
					t.Fatal(err)
				}
			} else {
				mustAuthorize(t, err)
			}

			passRoleInspector(t, s, operation.name, mode, "session policy")
		})
	}
}

func TestIAMPassRoleInstanceProfiles(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, true)
	ctx := context.Background()
	root := iamClient(s, "test", "us-east-1")
	role := boundaryRole(t, root, nil)
	profile, err := root.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{InstanceProfileName: new("passed")})
	mustAuthorize(t, err)
	key := authorizationUser(t, s)
	authorizationPolicy(t, s, passRoleDocument(`{"Effect":"Allow",`+
		`"Action":["ec2:*","iam:AddRoleToInstanceProfile"],"Resource":"*"}`))
	userIAM := iam.New(iam.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL),
		Credentials: authorizationProvider(key), Retryer: aws.NopRetryer{},
	})
	// Membership is not a PassRole operation.
	_, err = userIAM.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: new("passed"), RoleName: new("bounded"),
	})
	mustAuthorize(t, err)
	client := ec2.New(ec2.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL),
		Credentials: authorizationProvider(key), Retryer: aws.NopRetryer{},
	})
	image := latestImage(t, s)
	for _, spec := range []*ec2types.IamInstanceProfileSpecification{
		{Name: new("passed")}, {Arn: profile.InstanceProfile.Arn},
	} {
		_, err = client.RunInstances(ctx, &ec2.RunInstancesInput{
			ImageId: image, MinCount: new(int32(1)), MaxCount: new(int32(1)), IamInstanceProfile: spec,
		})
		wantCode(t, err, "AccessDenied")
		if !strings.Contains(err.Error(), "iam:PassRole") || !strings.Contains(err.Error(), *role) {
			t.Fatal(err)
		}
	}

	launched, err := ec2Client(s, "test", "us-east-1").RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: image, MinCount: new(int32(1)), MaxCount: new(int32(1)),
	})
	mustAuthorize(t, err)
	attach := &ec2.AssociateIamInstanceProfileInput{
		InstanceId:         launched.Instances[0].InstanceId,
		IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{Name: new("passed")},
	}
	_, err = client.AssociateIamInstanceProfile(ctx, attach)
	wantCode(t, err, "AccessDenied")
	if !strings.Contains(err.Error(), *role) {
		t.Fatal(err)
	}

	authorizationPolicy(t, s, passRoleDocument(`{"Effect":"Allow","Action":"ec2:*","Resource":"*"}`,
		fmt.Sprintf(`{"Effect":"Allow","Action":"iam:PassRole","Resource":%q,`+
			`"Condition":{"StringEquals":{"iam:PassedToService":"ec2.amazonaws.com"}}}`, *role)))
	_, err = client.AssociateIamInstanceProfile(ctx, attach)
	mustAuthorize(t, err)
	_, err = client.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: image, MinCount: new(int32(1)), MaxCount: new(int32(1)),
		IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{Arn: profile.InstanceProfile.Arn},
	})
	mustAuthorize(t, err)
}

func TestIAMPassRoleEveryRole(t *testing.T) {
	for _, service := range []string{"ecs", "eventbridge"} {
		t.Run(service, func(t *testing.T) {
			s := authorizationServer(t, config.AuthorizationEnforce, true)
			ctx := context.Background()
			key := authorizationUser(t, s)
			authorizationPolicy(t, s, passRoleDocument(passRolePrimary,
				`{"Effect":"Allow","Action":"iam:PassRole","Resource":"`+passedRoleARN+`"}`))
			credentials := authorizationProvider(key)
			other := "arn:aws:iam::000000000000:role/other"
			var err error
			if service == "ecs" {
				client := ecs.New(ecs.Options{
					Region: "us-east-1", BaseEndpoint: new(s.URL),
					Credentials: credentials, Retryer: aws.NopRetryer{},
				})
				_, err = client.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
					Family:           new("passed"),
					TaskRoleArn:      new(passedRoleARN),
					ExecutionRoleArn: new(other),
					ContainerDefinitions: []ecstypes.ContainerDefinition{
						{Name: new("web"), Image: new("nginx"), Memory: new(int32(128))},
					},
				})
				listed, listErr := ecsClient(s, "us-east-1").ListTaskDefinitions(ctx, &ecs.ListTaskDefinitionsInput{})
				mustAuthorize(t, listErr)
				if len(listed.TaskDefinitionArns) != 0 {
					t.Fatal("denied task was created")
				}
			} else {
				root := eventsClient(s, "test")
				_, ruleErr := root.PutRule(ctx, &eventbridge.PutRuleInput{
					Name: new("passed"), EventPattern: new(orderPattern),
				})
				mustAuthorize(t, ruleErr)
				client := eventbridge.New(eventbridge.Options{
					Region: "us-east-1", BaseEndpoint: new(s.URL),
					Credentials: credentials, Retryer: aws.NopRetryer{},
				})
				_, err = client.PutTargets(
					ctx,
					&eventbridge.PutTargetsInput{Rule: new("passed"), Targets: []ebtypes.Target{
						{
							Id:      new("one"),
							Arn:     new("arn:aws:sqs:us-east-1:000000000000:one"),
							RoleArn: new(passedRoleARN),
						},
						{Id: new("two"), Arn: new("arn:aws:sqs:us-east-1:000000000000:two"), RoleArn: new(other)},
					}},
				)
				listed, listErr := root.ListTargetsByRule(ctx, &eventbridge.ListTargetsByRuleInput{Rule: new("passed")})
				mustAuthorize(t, listErr)
				if len(listed.Targets) != 0 {
					t.Fatal("denied targets were added")
				}
			}

			wantCode(t, err, "AccessDeniedException")
			if !strings.Contains(err.Error(), other) {
				t.Fatal(err)
			}
		})
	}
}

func TestIAMPassRoleUpdates(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, true)
	ctx := context.Background()
	rootLambda := lambdaClient(s, "test")
	createFunction(t, rootLambda, "passed-update", "app.echo")
	machine := createMachine(t, sfnClient(s), "passed-update",
		`{"StartAt":"Done","States":{"Done":{"Type":"Succeed"}}}`, sfntypes.StateMachineTypeStandard)
	key := authorizationUser(t, s)
	credentials := authorizationProvider(key)
	authorizationPolicy(t, s, passRoleDocument(passRolePrimary))
	functions := lambda.New(lambda.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL),
		Credentials: credentials, Retryer: aws.NopRetryer{},
	})
	states := sfn.New(sfn.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL),
		Credentials: credentials, Retryer: aws.NopRetryer{},
	})
	_, err := functions.UpdateFunctionConfiguration(ctx, &lambda.UpdateFunctionConfigurationInput{
		FunctionName: new("passed-update"), Role: new(passedRoleARN),
	})
	wantCode(t, err, "AccessDeniedException")
	passRoleInspector(t, s, "UpdateFunctionConfiguration", config.AuthorizationEnforce, "identity-based policy")
	_, err = states.UpdateStateMachine(ctx, &sfn.UpdateStateMachineInput{
		StateMachineArn: machine, RoleArn: new(passedRoleARN),
	})
	wantCode(t, err, "AccessDeniedException")
	passRoleInspector(t, s, "UpdateStateMachine", config.AuthorizationEnforce, "identity-based policy")
	// Updates that omit roles do not introduce a PassRole requirement.
	_, err = functions.UpdateFunctionConfiguration(ctx, &lambda.UpdateFunctionConfigurationInput{
		FunctionName: new("passed-update"), Description: new("no role change"),
	})
	mustAuthorize(t, err)
	_, err = states.UpdateStateMachine(ctx, &sfn.UpdateStateMachineInput{
		StateMachineArn: machine, Definition: new(`{"StartAt":"Done","States":{"Done":{"Type":"Succeed"}}}`),
	})
	mustAuthorize(t, err)
	authorizationPolicy(t, s, passRoleDocument(passRolePrimary,
		`{"Effect":"Allow","Action":"iam:PassRole","Resource":"`+passedRoleARN+`"}`))
	_, err = functions.UpdateFunctionConfiguration(ctx, &lambda.UpdateFunctionConfigurationInput{
		FunctionName: new("passed-update"), Role: new(passedRoleARN),
	})
	mustAuthorize(t, err)
	_, err = states.UpdateStateMachine(ctx, &sfn.UpdateStateMachineInput{
		StateMachineArn: machine, RoleArn: new(passedRoleARN),
	})
	mustAuthorize(t, err)
}

func TestIAMPassRolePrimaryDenied(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, true)
	key := authorizationUser(t, s)
	authorizationPolicy(t, s, passRoleDocument(passRoleAllow("ecs-tasks.amazonaws.com")))
	operation := passRoleOperations(t)[1]
	err := operation.invoke(context.Background(), s, authorizationProvider(key))
	wantCode(t, err, "AccessDeniedException")
	if strings.Contains(err.Error(), "iam:PassRole") || !strings.Contains(err.Error(), "ecs:RegisterTaskDefinition") {
		t.Fatal(err)
	}
}
