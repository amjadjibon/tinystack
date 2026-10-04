package integration_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cip "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	ciptypes "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"

	"github.com/amjadjibon/tinystack/internal/services/lambda"
)

// triggerEvent is the part of a Cognito trigger's event the tests read.
type triggerEvent struct {
	CallerContext map[string]string `json:"callerContext"`
	Response      map[string]any    `json:"response"`
	Version       string            `json:"version"`
	TriggerSource string            `json:"triggerSource"`
	UserPoolID    string            `json:"userPoolId"`
	UserName      string            `json:"userName"`
	Request       struct {
		UserAttributes map[string]string `json:"userAttributes"`
		ValidationData map[string]string `json:"validationData"`
		ClientMetadata map[string]string `json:"clientMetadata"`
		Scopes         []string          `json:"scopes"`
	} `json:"request"`
}

// cognitoTrigger is the "app.cognito" handler: pre sign-up confirms and
// verifies users unless their validation data says "deny", pre token
// generation adds claims for both versions, and other triggers return the
// event.
func cognitoTrigger(payload []byte) (lambda.Result, error) {
	var ev map[string]any
	if err := json.Unmarshal(payload, &ev); err != nil {
		return lambda.Result{}, err
	}

	req, _ := ev["request"].(map[string]any)
	switch source, _ := ev["triggerSource"].(string); {
	case source == "PreSignUp_SignUp":
		if data, _ := req["validationData"].(map[string]any); data["deny"] != nil {
			return lambda.Result{Payload: []byte(`{"errorMessage":"not on the list"}`), Error: true}, nil
		}

		ev["response"] = map[string]any{"autoConfirmUser": true, "autoVerifyEmail": true}
	case ev["version"] == "1" && len(source) > 16 && source[:16] == "TokenGeneration_":
		ev["response"] = map[string]any{"claimsOverrideDetails": map[string]any{
			"claimsToAddOrOverride": map[string]any{"tier": "gold", "sub": "forged"},
			"claimsToSuppress":      []string{"email"},
			"groupOverrideDetails":  map[string]any{"groupsToOverride": []string{"vip"}},
		}}
	case len(source) > 16 && source[:16] == "TokenGeneration_":
		ev["response"] = map[string]any{"claimsAndScopeOverrideDetails": map[string]any{
			"idTokenGeneration": map[string]any{"claimsToAddOrOverride": map[string]any{"tenant": "acme"}},
			"accessTokenGeneration": map[string]any{
				"claimsToAddOrOverride": map[string]any{"tenant": "acme"}, "scopesToAdd": []string{"orders/read"},
			},
		}}
	}

	body, err := json.Marshal(ev)
	return lambda.Result{Payload: body}, err
}

// triggerCalls returns the Cognito events invoked so far, in order.
func triggerCalls(t *testing.T, calls chan invocation) []triggerEvent {
	t.Helper()
	var out []triggerEvent
	for {
		select {
		case call := <-calls:
			var ev triggerEvent
			if err := json.Unmarshal(call.payload, &ev); err != nil {
				t.Fatal(err)
			}

			out = append(out, ev)
		default:
			return out
		}
	}
}

func sources(events []triggerEvent) []string {
	var out []string
	for i := range events {
		out = append(out, events[i].TriggerSource)
	}

	return out
}

func TestCognitoTriggers(t *testing.T) {
	runtime := &fakeRuntime{calls: make(chan invocation, 20)}
	s := serverWith(t, runtime)
	ctx := context.Background()
	admin := cognitoClient(s, "test", "us-east-1")
	users := anonymousCognito(s)
	functionARN := aws.ToString(createFunction(t, lambdaClient(s, "test"), "cognito", "app.cognito").FunctionArn)
	failing := aws.ToString(createFunction(t, lambdaClient(s, "test"), "broken", "app.fail").FunctionArn)

	_, err := admin.CreateUserPool(ctx, &cip.CreateUserPoolInput{
		PoolName: aws.String("bad"), LambdaConfig: &ciptypes.LambdaConfigType{
			PreTokenGeneration: aws.String(functionARN),
			PreTokenGenerationConfig: &ciptypes.PreTokenGenerationVersionConfigType{
				LambdaArn: aws.String(failing), LambdaVersion: ciptypes.PreTokenGenerationLambdaVersionTypeV20,
			},
		},
	})
	code(t, err, "InvalidParameterException")

	// Triggers TinyStack does not run are rejected.
	if _, err = admin.CreateUserPool(ctx, &cip.CreateUserPoolInput{
		PoolName: aws.String("bad"), LambdaConfig: &ciptypes.LambdaConfigType{CustomMessage: aws.String(functionARN)},
	}); err == nil {
		t.Fatal("CustomMessage trigger accepted")
	}

	pool := createPool(t, admin, "triggered", &cip.CreateUserPoolInput{
		UsernameAttributes: []ciptypes.UsernameAttributeType{ciptypes.UsernameAttributeTypeEmail},
		LambdaConfig: &ciptypes.LambdaConfigType{
			PreSignUp: aws.String(functionARN), PostConfirmation: aws.String(functionARN),
			PostAuthentication: aws.String(functionARN), PreTokenGeneration: aws.String(functionARN),
		},
	})
	described, err := admin.DescribeUserPool(ctx, &cip.DescribeUserPoolInput{UserPoolId: pool.Id})
	if cfg := described.UserPool.LambdaConfig; err != nil || cfg == nil || cfg.PreTokenGenerationConfig == nil ||
		cfg.PreTokenGenerationConfig.LambdaVersion != ciptypes.PreTokenGenerationLambdaVersionTypeV10 {
		t.Fatalf("lambda config = %+v, %v", described, err)
	}

	client, err := admin.CreateUserPoolClient(ctx, &cip.CreateUserPoolClientInput{
		UserPoolId: pool.Id, ClientName: aws.String("app"),
		ExplicitAuthFlows: []ciptypes.ExplicitAuthFlowsType{
			ciptypes.ExplicitAuthFlowsTypeAllowUserPasswordAuth, ciptypes.ExplicitAuthFlowsTypeAllowRefreshTokenAuth,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	clientID := client.UserPoolClient.ClientId
	_, err = users.SignUp(ctx, &cip.SignUpInput{
		ClientId: clientID, Username: aws.String("mal@example.com"), Password: aws.String("Sup3r-Secret"),
		ValidationData: attrs("deny", "yes"),
	})
	code(t, err, "UserLambdaValidationException")
	if events := triggerCalls(t, runtime.calls); len(events) != 1 || events[0].Request.ValidationData["deny"] != "yes" {
		t.Fatalf("denied sign-up events = %+v", events)
	}

	// The trigger confirms the user and verifies the email address, so no code
	// is sent and post confirmation runs.
	signedUp, err := users.SignUp(ctx, &cip.SignUpInput{
		ClientId: clientID, Username: aws.String("ann@example.com"), Password: aws.String("Sup3r-Secret"),
		ClientMetadata: map[string]string{"campaign": "spring"},
	})
	if err != nil || !signedUp.UserConfirmed || signedUp.CodeDeliveryDetails != nil {
		t.Fatalf("sign-up = %+v, %v", signedUp, err)
	}

	events := triggerCalls(t, runtime.calls)
	if !slices.Equal(sources(events), []string{"PreSignUp_SignUp", "PostConfirmation_ConfirmSignUp"}) {
		t.Fatalf("sign-up events = %v", sources(events))
	}

	pre, post := events[0], events[1]
	if pre.UserName != aws.ToString(signedUp.UserSub) || pre.UserPoolID != aws.ToString(pool.Id) ||
		pre.CallerContext["clientId"] != *clientID || pre.Request.UserAttributes["email"] != "ann@example.com" ||
		pre.Request.ClientMetadata["campaign"] != "spring" || post.UserName != pre.UserName ||
		post.Request.UserAttributes["cognito:user_status"] != "CONFIRMED" ||
		post.Request.UserAttributes["email_verified"] != "true" {
		t.Fatalf("pre = %+v, post = %+v", pre, post)
	}

	signIn := func() *ciptypes.AuthenticationResultType {
		t.Helper()
		out, signErr := users.InitiateAuth(ctx, &cip.InitiateAuthInput{
			ClientId: clientID, AuthFlow: ciptypes.AuthFlowTypeUserPasswordAuth,
			AuthParameters: map[string]string{"USERNAME": "ann@example.com", "PASSWORD": "Sup3r-Secret"},
		})
		if signErr != nil {
			t.Fatal(signErr)
		}

		return out.AuthenticationResult
	}

	// Version 1 changes the ID token, leaving reserved claims alone, and the
	// groups of both.
	result := signIn()
	id, access := verifiedClaims(t, *result.IdToken), verifiedClaims(t, *result.AccessToken)
	groups, _ := access["cognito:groups"].([]any)
	if id["tier"] != "gold" || id["email"] != nil || id["sub"] != aws.ToString(signedUp.UserSub) ||
		access["tier"] != nil || len(groups) != 1 || groups[0] != "vip" {
		t.Fatalf("id = %+v, access = %+v", id, access)
	}

	events = triggerCalls(t, runtime.calls)
	if !slices.Equal(sources(events), []string{"TokenGeneration_Authentication", "PostAuthentication_Authentication"}) {
		t.Fatalf("sign-in events = %v", sources(events))
	}

	// Refreshing runs pre token generation, but not post authentication.
	refreshed, err := users.InitiateAuth(ctx, &cip.InitiateAuthInput{
		ClientId: clientID, AuthFlow: ciptypes.AuthFlowTypeRefreshTokenAuth,
		AuthParameters: map[string]string{"REFRESH_TOKEN": *result.RefreshToken},
	})
	if err != nil || verifiedClaims(t, *refreshed.AuthenticationResult.IdToken)["tier"] != "gold" {
		t.Fatalf("refreshed = %+v, %v", refreshed, err)
	}

	if got := sources(triggerCalls(t, runtime.calls)); !slices.Equal(got, []string{"TokenGeneration_RefreshTokens"}) {
		t.Fatalf("refresh events = %v", got)
	}

	// Version 2 can change the access token and its scopes.
	if _, err = admin.UpdateUserPool(ctx, &cip.UpdateUserPoolInput{
		UserPoolId: pool.Id, LambdaConfig: &ciptypes.LambdaConfigType{
			PreTokenGenerationConfig: &ciptypes.PreTokenGenerationVersionConfigType{
				LambdaArn: aws.String(functionARN), LambdaVersion: ciptypes.PreTokenGenerationLambdaVersionTypeV20,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	result = signIn()
	id, access = verifiedClaims(t, *result.IdToken), verifiedClaims(t, *result.AccessToken)
	if id["tenant"] != "acme" || access["tenant"] != "acme" ||
		access["scope"] != "aws.cognito.signin.user.admin orders/read" {
		t.Fatalf("id = %+v, access = %+v", id, access)
	}

	if events = triggerCalls(t, runtime.calls); len(events) != 1 || events[0].Version != "2" ||
		!slices.Equal(events[0].Request.Scopes, []string{"aws.cognito.signin.user.admin"}) {
		t.Fatalf("version 2 events = %+v", events)
	}

	// A trigger that raises fails the operation.
	if _, err = admin.UpdateUserPool(ctx, &cip.UpdateUserPoolInput{
		UserPoolId: pool.Id, LambdaConfig: &ciptypes.LambdaConfigType{PreTokenGeneration: aws.String(failing)},
	}); err != nil {
		t.Fatal(err)
	}

	_, err = users.InitiateAuth(ctx, &cip.InitiateAuthInput{
		ClientId: clientID, AuthFlow: ciptypes.AuthFlowTypeUserPasswordAuth,
		AuthParameters: map[string]string{"USERNAME": "ann@example.com", "PASSWORD": "Sup3r-Secret"},
	})
	code(t, err, "UserLambdaValidationException")
	triggerCalls(t, runtime.calls)

	// Confirming a user an administrator created runs post confirmation.
	if _, err = admin.UpdateUserPool(ctx, &cip.UpdateUserPoolInput{
		UserPoolId: pool.Id, LambdaConfig: &ciptypes.LambdaConfigType{PostConfirmation: aws.String(functionARN)},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = users.SignUp(ctx, &cip.SignUpInput{
		ClientId: clientID, Username: aws.String("bo@example.com"), Password: aws.String("Sup3r-Secret"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = admin.AdminConfirmSignUp(ctx, &cip.AdminConfirmSignUpInput{
		UserPoolId: pool.Id, Username: aws.String("bo@example.com"),
	}); err != nil {
		t.Fatal(err)
	}

	if events = triggerCalls(t, runtime.calls); len(events) != 1 ||
		events[0].TriggerSource != "PostConfirmation_ConfirmSignUp" ||
		events[0].CallerContext["clientId"] != "CLIENT_ID_NOT_APPLICABLE" {
		t.Fatalf("admin confirm events = %+v", events)
	}
}
