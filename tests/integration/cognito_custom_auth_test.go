package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	cip "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	ciptypes "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"

	"github.com/amjadjibon/tinystack/internal/services/lambda"
)

type customAuthRuntime struct {
	admin  *cip.Client
	events []map[string]any
	fakeRuntime
	mu sync.Mutex
}

func (r *customAuthRuntime) Invoke(ctx context.Context, fn *lambda.Function, payload []byte) (lambda.Result, error) {
	var ev map[string]any
	if err := json.Unmarshal(payload, &ev); err != nil {
		return lambda.Result{}, err
	}

	// Calling Cognito from the trigger proves no store transaction is held during invocation.
	if _, err := r.admin.GetUserPoolMfaConfig(
		ctx,
		&cip.GetUserPoolMfaConfigInput{UserPoolId: new(ev["userPoolId"].(string))},
	); err != nil {
		return lambda.Result{}, err
	}

	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	if fn.Handler == "app.invalid" {
		return lambda.Result{Payload: []byte(`{"response":{}}`)}, nil
	}

	req := ev["request"].(map[string]any)
	var response map[string]any
	switch ev["triggerSource"] {
	case "DefineAuthChallenge_Authentication":
		history := req["session"].([]any)
		name, failed, completed := "CUSTOM_CHALLENGE", false, false
		rounds := 0
		for _, raw := range history {
			result := raw.(map[string]any)
			failed = failed || !result["challengeResult"].(bool)
			if result["challengeName"] == "CUSTOM_CHALLENGE" {
				rounds++
			}
		}

		if len(history) == 1 && history[0].(map[string]any)["challengeName"] == "SRP_A" {
			name = "PASSWORD_VERIFIER"
		}

		completed = rounds == 2 && !failed
		response = map[string]any{"challengeName": name, "issueTokens": completed, "failAuthentication": failed}
	case "CreateAuthChallenge_Authentication":
		rounds := 1
		for _, raw := range req["session"].([]any) {
			if raw.(map[string]any)["challengeName"] == "CUSTOM_CHALLENGE" {
				rounds++
			}
		}

		response = map[string]any{
			"publicChallengeParameters":  map[string]string{"question": fmt.Sprintf("round %d", rounds)},
			"privateChallengeParameters": map[string]string{"answer": fmt.Sprintf("answer %d", rounds)},
			"challengeMetadata":          fmt.Sprintf("round-%d", rounds),
		}
	case "VerifyAuthChallengeResponse_Authentication":
		private := req["privateChallengeParameters"].(map[string]any)
		response = map[string]any{"answerCorrect": req["challengeAnswer"] == private["answer"]}
	default:
		return lambda.Result{}, fmt.Errorf("unexpected trigger source %v", ev["triggerSource"])
	}

	ev["response"] = response
	data, err := json.Marshal(ev)
	return lambda.Result{Payload: data}, err
}

func (r *customAuthRuntime) snapshots() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func TestCognitoCustomAuthentication(t *testing.T) {
	runtime := &customAuthRuntime{}
	s := serverWith(t, runtime)
	ctx := context.Background()
	admin, users := cognitoClient(s, "test", "us-east-1"), anonymousCognito(s)
	runtime.admin = admin
	l := lambdaClient(s, "test")
	define := createFunction(t, l, "define", "app.define").FunctionArn
	create := createFunction(t, l, "create", "app.create").FunctionArn
	verify := createFunction(t, l, "verify", "app.verify").FunctionArn
	pool := createPool(t, admin, "custom", &cip.CreateUserPoolInput{LambdaConfig: &ciptypes.LambdaConfigType{
		DefineAuthChallenge: define, CreateAuthChallenge: create, VerifyAuthChallengeResponse: verify,
	}})
	clientID := mfaClient(t, admin, pool.Id, "app")
	createMFAUser(t, admin, pool.Id, "alice")
	start := func(params map[string]string) *cip.InitiateAuthOutput {
		t.Helper()
		out, err := users.InitiateAuth(ctx, &cip.InitiateAuthInput{
			ClientId: clientID, AuthFlow: ciptypes.AuthFlowTypeCustomAuth,
			AuthParameters: params, ClientMetadata: map[string]string{"initial": "not forwarded"},
		})
		if err != nil {
			t.Fatal(err)
		}

		return out
	}

	started := start(map[string]string{"USERNAME": "alice"})
	if started.ChallengeName != ciptypes.ChallengeNameTypeCustomChallenge ||
		started.ChallengeParameters["question"] != "round 1" ||
		started.ChallengeParameters["answer"] != "" {
		t.Fatalf("custom = %+v", started)
	}

	respond := func(session *string, answer string) (*cip.RespondToAuthChallengeOutput, error) {
		return users.RespondToAuthChallenge(ctx, &cip.RespondToAuthChallengeInput{
			ClientId:      clientID,
			Session:       session,
			ChallengeName: ciptypes.ChallengeNameTypeCustomChallenge,
			ChallengeResponses: map[string]string{
				"USERNAME": "alice",
				"ANSWER":   answer,
			},
			ClientMetadata: map[string]string{"round": "respond"},
		})
	}

	second, err := respond(started.Session, "answer 1")
	if err != nil || second.ChallengeName != ciptypes.ChallengeNameTypeCustomChallenge ||
		second.ChallengeParameters["question"] != "round 2" {
		t.Fatalf("round two = %+v, %v", second, err)
	}

	_, err = respond(started.Session, "answer 1")
	code(t, err, "NotAuthorizedException")
	complete, err := respond(second.Session, "answer 2")
	if err != nil || complete.AuthenticationResult == nil {
		t.Fatalf("complete = %+v, %v", complete, err)
	}

	if claim := verifiedClaims(t, *complete.AuthenticationResult.IdToken); claim["cognito:username"] != "alice" {
		t.Fatalf("claims = %+v", claim)
	}

	failed := start(map[string]string{"USERNAME": "alice"})
	_, err = respond(failed.Session, "wrong")
	code(t, err, "NotAuthorizedException")
	_, err = respond(failed.Session, "answer 1")
	code(t, err, "NotAuthorizedException")

	events := runtime.snapshots()
	expected := []string{
		"DefineAuthChallenge_Authentication",
		"CreateAuthChallenge_Authentication",
		"VerifyAuthChallengeResponse_Authentication",
		"DefineAuthChallenge_Authentication",
		"CreateAuthChallenge_Authentication",
		"VerifyAuthChallengeResponse_Authentication",
		"DefineAuthChallenge_Authentication",
	}
	for i, source := range expected {
		if events[i]["triggerSource"] != source || events[i]["userPoolId"] != *pool.Id ||
			events[i]["userName"] != "alice" ||
			events[i]["callerContext"].(map[string]any)["clientId"] != *clientID {
			t.Fatalf("event %d = %+v", i, events[i])
		}
	}

	if events[0]["request"].(map[string]any)["clientMetadata"] != nil {
		t.Fatal("InitiateAuth metadata forwarded")
	}

	defineReq := events[3]["request"].(map[string]any)
	history := defineReq["session"].([]any)
	if len(history) != 1 || history[0].(map[string]any)["challengeMetadata"] != "round-1" ||
		history[0].(map[string]any)["challengeResult"] != true ||
		defineReq["clientMetadata"].(map[string]any)["round"] != "respond" {
		t.Fatalf("history and metadata = %+v", defineReq)
	}

	// A custom flow can start with SRP and continue through PASSWORD_VERIFIER into the same multi-round custom flow.
	srp := newSRPClient(*pool.Id)
	srpStart := start(map[string]string{"USERNAME": "alice", "CHALLENGE_NAME": "SRP_A", "SRP_A": srp.bigA.Text(16)})
	if srpStart.ChallengeName != ciptypes.ChallengeNameTypePasswordVerifier {
		t.Fatalf("custom SRP = %+v", srpStart)
	}

	custom, err := users.RespondToAuthChallenge(ctx, &cip.RespondToAuthChallengeInput{
		ClientId:           clientID,
		Session:            srpStart.Session,
		ChallengeName:      srpStart.ChallengeName,
		ChallengeResponses: srp.claim(srpStart.ChallengeParameters, "Perm4nent!"),
	})
	if err != nil || custom.ChallengeName != ciptypes.ChallengeNameTypeCustomChallenge {
		t.Fatalf("SRP to custom = %+v, %v", custom, err)
	}

	roundTwo, err := respond(custom.Session, "answer 1")
	if err != nil {
		t.Fatal(err)
	}

	if _, err = respond(roundTwo.Session, "answer 2"); err != nil {
		t.Fatal(err)
	}

	plain := createPool(t, admin, "no triggers", nil)
	plainClient := mfaClient(t, admin, plain.Id, "app")
	_, err = users.InitiateAuth(ctx, &cip.InitiateAuthInput{
		ClientId: plainClient, AuthFlow: ciptypes.AuthFlowTypeCustomAuth,
		AuthParameters: map[string]string{"USERNAME": "alice"},
	})
	code(t, err, "InvalidParameterException")

	// A client that hides user existence still invokes all three triggers with userNotFound=true.
	hidden, hiddenErr := admin.CreateUserPoolClient(ctx, &cip.CreateUserPoolClientInput{
		UserPoolId: pool.Id,
		ClientName: new("hidden"), PreventUserExistenceErrors: ciptypes.PreventUserExistenceErrorTypesEnabled,
		ExplicitAuthFlows: []ciptypes.ExplicitAuthFlowsType{ciptypes.ExplicitAuthFlowsTypeAllowCustomAuth},
	})
	if hiddenErr != nil {
		t.Fatal(hiddenErr)
	}

	absent, absentErr := users.InitiateAuth(ctx, &cip.InitiateAuthInput{
		ClientId: hidden.UserPoolClient.ClientId,
		AuthFlow: ciptypes.AuthFlowTypeCustomAuth, AuthParameters: map[string]string{"USERNAME": "absent"},
	})
	if absentErr != nil || absent.ChallengeName != ciptypes.ChallengeNameTypeCustomChallenge {
		t.Fatalf("hidden nonexistent user = %+v, %v", absent, absentErr)
	}

	_, absentErr = users.RespondToAuthChallenge(ctx, &cip.RespondToAuthChallengeInput{
		ClientId:           hidden.UserPoolClient.ClientId,
		Session:            absent.Session,
		ChallengeName:      absent.ChallengeName,
		ChallengeResponses: map[string]string{"USERNAME": "absent", "ANSWER": "wrong"},
	})
	code(t, absentErr, "NotAuthorizedException")
	for _, ev := range runtime.snapshots() {
		if ev["userName"] == "absent" && ev["request"].(map[string]any)["userNotFound"] != true {
			t.Fatalf("missing userNotFound = %+v", ev)
		}
	}

	// An incomplete DefineAuthChallenge response fails explicitly instead of issuing tokens.
	invalid := createFunction(t, l, "invalid", "app.invalid").FunctionArn
	badPool := createPool(
		t,
		admin,
		"invalid response",
		&cip.CreateUserPoolInput{LambdaConfig: &ciptypes.LambdaConfigType{
			DefineAuthChallenge: invalid, CreateAuthChallenge: create, VerifyAuthChallengeResponse: verify,
		}},
	)
	badClient := mfaClient(t, admin, badPool.Id, "app")
	createMFAUser(t, admin, badPool.Id, "alice")
	_, err = users.InitiateAuth(ctx, &cip.InitiateAuthInput{
		ClientId: badClient, AuthFlow: ciptypes.AuthFlowTypeCustomAuth,
		AuthParameters: map[string]string{"USERNAME": "alice"},
	})
	code(t, err, "InvalidLambdaResponseException")
}

func TestCognitoCustomSRPWithMFAAndNewPassword(t *testing.T) {
	runtime := &customAuthRuntime{}
	s := serverWith(t, runtime)
	ctx := context.Background()
	admin, users := cognitoClient(s, "test", "us-east-1"), anonymousCognito(s)
	runtime.admin = admin
	l := lambdaClient(s, "test")
	function := createFunction(t, l, "custom", "app.custom").FunctionArn
	pool := createPool(t, admin, "custom with MFA", &cip.CreateUserPoolInput{
		SmsConfiguration: &ciptypes.SmsConfigurationType{SnsCallerArn: new("arn:aws:iam::000000000000:role/sms")},
		LambdaConfig: &ciptypes.LambdaConfigType{
			DefineAuthChallenge: function, CreateAuthChallenge: function,
			VerifyAuthChallengeResponse: function,
		},
	})
	clientID := mfaClient(t, admin, pool.Id, "app")
	if _, err := admin.AdminCreateUser(ctx, &cip.AdminCreateUserInput{
		UserPoolId: pool.Id, Username: new("alice"),
		TemporaryPassword: new("Temp0rary!"), MessageAction: ciptypes.MessageActionTypeSuppress,
		UserAttributes: attrs("phone_number", "+15555550123"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := admin.SetUserPoolMfaConfig(ctx, &cip.SetUserPoolMfaConfigInput{
		UserPoolId:       pool.Id,
		MfaConfiguration: ciptypes.UserPoolMfaTypeOn,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := admin.AdminSetUserMFAPreference(ctx, &cip.AdminSetUserMFAPreferenceInput{
		UserPoolId: pool.Id, Username: new("alice"),
		SMSMfaSettings: &ciptypes.SMSMfaSettingsType{Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}

	srp := newSRPClient(*pool.Id)
	started, err := users.InitiateAuth(ctx, &cip.InitiateAuthInput{
		ClientId: clientID, AuthFlow: ciptypes.AuthFlowTypeCustomAuth,
		AuthParameters: map[string]string{"USERNAME": "alice", "CHALLENGE_NAME": "SRP_A", "SRP_A": srp.bigA.Text(16)},
	})
	if err != nil {
		t.Fatal(err)
	}

	password, err := users.RespondToAuthChallenge(ctx, &cip.RespondToAuthChallengeInput{
		ClientId: clientID, Session: started.Session,
		ChallengeName: started.ChallengeName, ChallengeResponses: srp.claim(started.ChallengeParameters, "Temp0rary!"),
	})
	if err != nil || password.ChallengeName != ciptypes.ChallengeNameTypeNewPasswordRequired {
		t.Fatalf("new password = %+v, %v", password, err)
	}

	sms, err := users.RespondToAuthChallenge(ctx, &cip.RespondToAuthChallengeInput{
		ClientId:           clientID,
		Session:            password.Session,
		ChallengeName:      password.ChallengeName,
		ChallengeResponses: map[string]string{"USERNAME": "alice", "NEW_PASSWORD": "Perm4nent!"},
	})
	if err != nil || sms.ChallengeName != ciptypes.ChallengeNameTypeSmsMfa {
		t.Fatalf("SMS after new password = %+v, %v", sms, err)
	}

	_, _, smsCode := cognitoMessage(t, s, "sms:+15555550123")
	custom, err := users.RespondToAuthChallenge(ctx, &cip.RespondToAuthChallengeInput{
		ClientId:           clientID,
		Session:            sms.Session,
		ChallengeName:      sms.ChallengeName,
		ChallengeResponses: map[string]string{"USERNAME": "alice", "SMS_MFA_CODE": smsCode},
	})
	if err != nil || custom.ChallengeName != ciptypes.ChallengeNameTypeCustomChallenge {
		t.Fatalf("custom after MFA = %+v, %v", custom, err)
	}

	for round := 1; round <= 2; round++ {
		custom, err = users.RespondToAuthChallenge(ctx, &cip.RespondToAuthChallengeInput{
			ClientId:           clientID,
			Session:            custom.Session,
			ChallengeName:      custom.ChallengeName,
			ChallengeResponses: map[string]string{"USERNAME": "alice", "ANSWER": fmt.Sprintf("answer %d", round)},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	if custom.AuthenticationResult == nil {
		t.Fatalf("complete = %+v", custom)
	}

	seen := false
	for _, event := range runtime.snapshots() {
		if event["triggerSource"] != "DefineAuthChallenge_Authentication" {
			continue
		}

		raw, _ := json.Marshal(event["request"])
		seen = seen ||
			(strings.Contains(string(raw), "NEW_PASSWORD_REQUIRED") && strings.Contains(string(raw), "SMS_MFA"))
	}

	if !seen {
		t.Fatal("custom history lost password change or MFA")
	}
}
