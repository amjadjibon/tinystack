package integration_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cip "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	ciptypes "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

// totpCode is the client authenticator's RFC 6238 SHA-1, six-digit, 30-second implementation.
func totpCode(t *testing.T, secret string, now time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}

	counter := make([]byte, 8)
	binary.BigEndian.PutUint64(counter, uint64(now.Unix()/30))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter)
	digest := mac.Sum(nil)
	offset := digest[len(digest)-1] & 0xf
	number := binary.BigEndian.Uint32(digest[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", number%1_000_000)
}

func createMFAUser(t *testing.T, admin *cip.Client, poolID *string, username string) {
	t.Helper()
	ctx := context.Background()
	if _, err := admin.AdminCreateUser(ctx, &cip.AdminCreateUserInput{
		UserPoolId: poolID, Username: new(username), MessageAction: ciptypes.MessageActionTypeSuppress,
		UserAttributes: attrs("phone_number", "+15555550123"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := admin.AdminSetUserPassword(ctx, &cip.AdminSetUserPasswordInput{
		UserPoolId: poolID, Username: new(username), Password: new("Perm4nent!"), Permanent: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func mfaClient(t *testing.T, admin *cip.Client, poolID *string, name string) *string {
	t.Helper()
	out, err := admin.CreateUserPoolClient(context.Background(), &cip.CreateUserPoolClientInput{
		UserPoolId: poolID, ClientName: new(name), ExplicitAuthFlows: []ciptypes.ExplicitAuthFlowsType{
			ciptypes.ExplicitAuthFlowsTypeAllowUserPasswordAuth,
			ciptypes.ExplicitAuthFlowsTypeAllowAdminUserPasswordAuth,
			ciptypes.ExplicitAuthFlowsTypeAllowUserSrpAuth,
			ciptypes.ExplicitAuthFlowsTypeAllowRefreshTokenAuth,
			ciptypes.ExplicitAuthFlowsTypeAllowCustomAuth,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	return out.UserPoolClient.ClientId
}

func startMFA(t *testing.T, users *cip.Client, clientID *string, username string) *cip.InitiateAuthOutput {
	t.Helper()
	out, err := users.InitiateAuth(context.Background(), &cip.InitiateAuthInput{
		ClientId: clientID, AuthFlow: ciptypes.AuthFlowTypeUserPasswordAuth,
		AuthParameters: map[string]string{"USERNAME": username, "PASSWORD": "Perm4nent!"},
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

func setupTOTP(t *testing.T, users *cip.Client, token *string) string {
	t.Helper()
	ctx := context.Background()
	association, err := users.AssociateSoftwareToken(ctx, &cip.AssociateSoftwareTokenInput{AccessToken: token})
	if err != nil {
		t.Fatal(err)
	}

	secret := aws.ToString(association.SecretCode)
	// Clock skew of one step is accepted.
	verified, err := users.VerifySoftwareToken(ctx, &cip.VerifySoftwareTokenInput{
		AccessToken:        token,
		UserCode:           new(totpCode(t, secret, time.Now().Add(-30*time.Second))),
		FriendlyDeviceName: new("phone"),
	})
	if err != nil || verified.Status != ciptypes.VerifySoftwareTokenResponseTypeSuccess {
		t.Fatalf("verify = %+v, %v", verified, err)
	}

	return secret
}

// setCognitoTime ages persisted records to exercise real SDK calls at expiry boundaries without sleeping three minutes.
func setCognitoTime(t *testing.T, store storage.Store, kind, poolID, id, field string, at time.Time) {
	t.Helper()
	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	key := scope.Prefix("cognitoidp", kind) + poolID + "/" + id
	err := store.Update(context.Background(), func(tx storage.WriteTx) error {
		record, found, readErr := storage.Read[map[string]any](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return fmt.Errorf("missing record %s", key)
		}

		(*record)[field] = at
		return storage.Write(tx, key, record)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCognitoTOTPAndSMSMFA(t *testing.T) {
	store := &memory.Store{}
	s, stop := serverStore(t, testConfig(), &fakeRuntime{}, store)
	t.Cleanup(stop)
	ctx := context.Background()
	admin, users := cognitoClient(s, "test", "us-east-1"), anonymousCognito(s)
	pool := createPool(t, admin, "mfa", &cip.CreateUserPoolInput{
		AliasAttributes:  []ciptypes.AliasAttributeType{ciptypes.AliasAttributeTypePhoneNumber},
		SmsConfiguration: &ciptypes.SmsConfigurationType{SnsCallerArn: new("arn:aws:iam::000000000000:role/sms")},
	})
	clientID := mfaClient(t, admin, pool.Id, "app")
	otherClient := mfaClient(t, admin, pool.Id, "other")
	createMFAUser(t, admin, pool.Id, "alice")
	createMFAUser(t, admin, pool.Id, "bob")
	if _, err := admin.SetUserPoolMfaConfig(ctx, &cip.SetUserPoolMfaConfigInput{
		UserPoolId:                    pool.Id,
		MfaConfiguration:              ciptypes.UserPoolMfaTypeOptional,
		SoftwareTokenMfaConfiguration: &ciptypes.SoftwareTokenMfaConfigType{Enabled: true},
		SmsMfaConfiguration: &ciptypes.SmsMfaConfigType{
			SmsAuthenticationMessage: new("Sign-in code: {####}"),
		},
	}); err != nil {
		t.Fatal(err)
	}

	baseline := startMFA(t, users, clientID, "alice")
	if baseline.AuthenticationResult == nil {
		t.Fatalf("optional unset = %+v", baseline)
	}

	secret := setupTOTP(t, users, baseline.AuthenticationResult.AccessToken)
	if _, err := users.SetUserMFAPreference(ctx, &cip.SetUserMFAPreferenceInput{
		AccessToken:              baseline.AuthenticationResult.AccessToken,
		SoftwareTokenMfaSettings: &ciptypes.SoftwareTokenMfaSettingsType{Enabled: true, PreferredMfa: true},
	}); err != nil {
		t.Fatal(err)
	}

	self, err := users.GetUser(ctx, &cip.GetUserInput{AccessToken: baseline.AuthenticationResult.AccessToken})
	if err != nil || self.PreferredMfaSetting == nil || *self.PreferredMfaSetting != "SOFTWARE_TOKEN_MFA" ||
		!slices.Equal(self.UserMFASettingList, []string{"SOFTWARE_TOKEN_MFA"}) {
		t.Fatalf("self = %+v, %v", self, err)
	}

	challenge := startMFA(t, users, clientID, "alice")
	if challenge.ChallengeName != ciptypes.ChallengeNameTypeSoftwareTokenMfa || challenge.AuthenticationResult != nil {
		t.Fatalf("totp challenge = %+v", challenge)
	}

	answer := &cip.RespondToAuthChallengeInput{
		ClientId: clientID, Session: challenge.Session, ChallengeName: challenge.ChallengeName,
		ChallengeResponses: map[string]string{"USERNAME": "alice", "SOFTWARE_TOKEN_MFA_CODE": "wrong!"},
	}
	_, err = users.RespondToAuthChallenge(ctx, answer)
	code(t, err, "CodeMismatchException")
	answer.ChallengeResponses["SOFTWARE_TOKEN_MFA_CODE"] = totpCode(t, secret, time.Now())
	answer.ClientId = otherClient
	_, err = users.RespondToAuthChallenge(ctx, answer)
	code(t, err, "NotAuthorizedException")
	answer.ClientId = clientID
	answer.ChallengeResponses["USERNAME"] = "bob"
	_, err = users.RespondToAuthChallenge(ctx, answer)
	code(t, err, "NotAuthorizedException")
	answer.ChallengeResponses["USERNAME"] = "alice"
	signed, err := users.RespondToAuthChallenge(ctx, answer)
	if err != nil || signed.AuthenticationResult == nil {
		t.Fatalf("signed = %+v, %v", signed, err)
	}

	// Wrong codes are limited per session: four still allow the right code, five end the session.
	for wrong, want := range map[int]string{4: "", 5: "NotAuthorizedException"} {
		retry := startMFA(t, users, clientID, "alice")
		attempt := &cip.RespondToAuthChallengeInput{
			ClientId: clientID, Session: retry.Session, ChallengeName: retry.ChallengeName,
			ChallengeResponses: map[string]string{"USERNAME": "alice", "SOFTWARE_TOKEN_MFA_CODE": "wrong!"},
		}
		for range wrong {
			_, err = users.RespondToAuthChallenge(ctx, attempt)
			code(t, err, "CodeMismatchException")
		}

		attempt.ChallengeResponses["SOFTWARE_TOKEN_MFA_CODE"] = totpCode(t, secret, time.Now())
		_, err = users.RespondToAuthChallenge(ctx, attempt)
		if want == "" && err != nil {
			t.Fatalf("after %d wrong codes: %v", wrong, err)
		}

		if want != "" {
			code(t, err, want)
		}
	}

	_, err = users.RespondToAuthChallenge(ctx, answer)
	code(t, err, "NotAuthorizedException")
	refreshed, err := users.InitiateAuth(ctx, &cip.InitiateAuthInput{
		ClientId:       clientID,
		AuthFlow:       ciptypes.AuthFlowTypeRefreshTokenAuth,
		AuthParameters: map[string]string{"REFRESH_TOKEN": *signed.AuthenticationResult.RefreshToken},
	})
	if err != nil || refreshed.AuthenticationResult == nil || refreshed.ChallengeName != "" {
		t.Fatalf("refresh = %+v, %v", refreshed, err)
	}

	aged := startMFA(t, users, clientID, "alice")
	setCognitoTime(t, store, "session", *pool.Id, *aged.Session, "Expires", time.Now().Add(-time.Second))
	answer.Session = aged.Session
	_, err = users.RespondToAuthChallenge(ctx, answer)
	code(t, err, "NotAuthorizedException")

	// SRP and the admin password flow both require MFA after checking the password.
	srp := newSRPClient(*pool.Id)
	srpStart, err := users.InitiateAuth(ctx, &cip.InitiateAuthInput{
		ClientId: clientID, AuthFlow: ciptypes.AuthFlowTypeUserSrpAuth,
		AuthParameters: map[string]string{"USERNAME": "alice", "SRP_A": srp.bigA.Text(16)},
	})
	if err != nil {
		t.Fatal(err)
	}

	srpNext, err := users.RespondToAuthChallenge(ctx, &cip.RespondToAuthChallengeInput{
		ClientId: clientID,
		Session:  srpStart.Session, ChallengeName: srpStart.ChallengeName,
		ChallengeResponses: srp.claim(srpStart.ChallengeParameters, "Perm4nent!"),
	})
	if err != nil || srpNext.ChallengeName != ciptypes.ChallengeNameTypeSoftwareTokenMfa {
		t.Fatalf("SRP MFA = %+v, %v", srpNext, err)
	}

	adminStart, err := admin.AdminInitiateAuth(ctx, &cip.AdminInitiateAuthInput{
		UserPoolId:     pool.Id,
		ClientId:       clientID,
		AuthFlow:       ciptypes.AuthFlowTypeAdminUserPasswordAuth,
		AuthParameters: map[string]string{"USERNAME": "alice", "PASSWORD": "Perm4nent!"},
	})
	if err != nil || adminStart.ChallengeName != ciptypes.ChallengeNameTypeSoftwareTokenMfa {
		t.Fatalf("admin MFA = %+v, %v", adminStart, err)
	}

	if _, err = admin.AdminRespondToAuthChallenge(ctx, &cip.AdminRespondToAuthChallengeInput{
		UserPoolId:    pool.Id,
		ClientId:      clientID,
		Session:       adminStart.Session,
		ChallengeName: adminStart.ChallengeName,
		ChallengeResponses: map[string]string{
			"USERNAME":                "alice",
			"SOFTWARE_TOKEN_MFA_CODE": totpCode(t, secret, time.Now()),
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Enabling both factors with no preference prompts for selection.
	if _, err = admin.AdminSetUserMFAPreference(ctx, &cip.AdminSetUserMFAPreferenceInput{
		UserPoolId: pool.Id, Username: new("alice"),
		SMSMfaSettings:           &ciptypes.SMSMfaSettingsType{Enabled: true},
		SoftwareTokenMfaSettings: &ciptypes.SoftwareTokenMfaSettingsType{Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}

	selected := startMFA(t, users, clientID, "alice")
	if selected.ChallengeName != ciptypes.ChallengeNameTypeSelectMfaType {
		t.Fatalf("selection = %+v", selected)
	}

	sms, err := users.RespondToAuthChallenge(ctx, &cip.RespondToAuthChallengeInput{
		ClientId:           clientID,
		Session:            selected.Session,
		ChallengeName:      selected.ChallengeName,
		ChallengeResponses: map[string]string{"USERNAME": "alice", "ANSWER": "SMS_MFA"},
	})
	if err != nil || sms.ChallengeName != ciptypes.ChallengeNameTypeSmsMfa {
		t.Fatalf("sms = %+v, %v", sms, err)
	}

	_, text, smsCode := cognitoMessage(t, s, "sms:+15555550123")
	if text != "Sign-in code: "+smsCode {
		t.Fatalf("SMS message = %q", text)
	}

	smsAnswer := &cip.RespondToAuthChallengeInput{
		ClientId: clientID, Session: sms.Session, ChallengeName: sms.ChallengeName,
		ChallengeResponses: map[string]string{"USERNAME": "alice", "SMS_MFA_CODE": "wrong"},
	}
	_, err = users.RespondToAuthChallenge(ctx, smsAnswer)
	code(t, err, "CodeMismatchException")
	smsAnswer.ChallengeResponses["SMS_MFA_CODE"] = smsCode
	if _, err = users.RespondToAuthChallenge(ctx, smsAnswer); err != nil {
		t.Fatal(err)
	}

	_, err = users.RespondToAuthChallenge(ctx, smsAnswer)
	code(t, err, "NotAuthorizedException")

	aliasSignIn := startMFA(t, users, clientID, "+15555550123")
	if aliasSignIn.ChallengeName != ciptypes.ChallengeNameTypeSelectMfaType ||
		aliasSignIn.ChallengeParameters["USERNAME"] != "alice" {
		t.Fatalf("verified phone alias = %+v", aliasSignIn)
	}

	// Invalid preference updates are atomic.
	_, err = admin.AdminSetUserMFAPreference(ctx, &cip.AdminSetUserMFAPreferenceInput{
		UserPoolId: pool.Id, Username: new("alice"),
		SMSMfaSettings:           &ciptypes.SMSMfaSettingsType{Enabled: true, PreferredMfa: true},
		SoftwareTokenMfaSettings: &ciptypes.SoftwareTokenMfaSettingsType{Enabled: true, PreferredMfa: true},
	})
	code(t, err, "InvalidParameterException")
	reported, err := admin.AdminGetUser(ctx, &cip.AdminGetUserInput{UserPoolId: pool.Id, Username: new("alice")})
	if err != nil || reported.PreferredMfaSetting != nil || len(reported.UserMFASettingList) != 2 {
		t.Fatalf("settings = %+v, %v", reported, err)
	}

	if _, err = admin.AdminSetUserSettings(ctx, &cip.AdminSetUserSettingsInput{
		UserPoolId: pool.Id,
		Username:   new("bob"),
		MFAOptions: []ciptypes.MFAOptionType{
			{DeliveryMedium: ciptypes.DeliveryMediumTypeSms, AttributeName: new("phone_number")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	legacy := startMFA(t, users, clientID, "bob")
	if legacy.ChallengeName != ciptypes.ChallengeNameTypeSmsMfa {
		t.Fatalf("legacy = %+v", legacy)
	}

	// An SMS code proves only the destination to which that challenge sent it.
	_, _, oldCode := cognitoMessage(t, s, "sms:+15555550123")
	if _, err = admin.AdminUpdateUserAttributes(ctx, &cip.AdminUpdateUserAttributesInput{
		UserPoolId: pool.Id, Username: new("bob"),
		UserAttributes: attrs("phone_number", "+15555550124"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = users.RespondToAuthChallenge(ctx, &cip.RespondToAuthChallengeInput{
		ClientId:           clientID,
		Session:            legacy.Session,
		ChallengeName:      legacy.ChallengeName,
		ChallengeResponses: map[string]string{"USERNAME": "bob", "SMS_MFA_CODE": oldCode},
	})
	code(t, err, "NotAuthorizedException")

	if _, err = admin.SetUserPoolMfaConfig(
		ctx,
		&cip.SetUserPoolMfaConfigInput{UserPoolId: pool.Id, MfaConfiguration: ciptypes.UserPoolMfaTypeOff},
	); err != nil {
		t.Fatal(err)
	}

	if off := startMFA(t, users, clientID, "alice"); off.AuthenticationResult == nil {
		t.Fatalf("OFF = %+v", off)
	}
}

func TestCognitoRequiredMFASetup(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	admin, users := cognitoClient(s, "test", "us-east-1"), anonymousCognito(s)
	pool := createPool(t, admin, "required setup", nil)
	clientID := mfaClient(t, admin, pool.Id, "app")
	createMFAUser(t, admin, pool.Id, "alice")
	if _, err := admin.SetUserPoolMfaConfig(ctx, &cip.SetUserPoolMfaConfigInput{
		UserPoolId:                    pool.Id,
		MfaConfiguration:              ciptypes.UserPoolMfaTypeOn,
		SoftwareTokenMfaConfiguration: &ciptypes.SoftwareTokenMfaConfigType{Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}

	started := startMFA(t, users, clientID, "alice")
	if started.ChallengeName != ciptypes.ChallengeNameTypeMfaSetup {
		t.Fatalf("setup = %+v", started)
	}

	_, err := users.RespondToAuthChallenge(ctx, &cip.RespondToAuthChallengeInput{
		ClientId: clientID, Session: started.Session,
		ChallengeName: started.ChallengeName, ChallengeResponses: map[string]string{"USERNAME": "alice"},
	})
	code(t, err, "NotAuthorizedException")
	associated, err := users.AssociateSoftwareToken(ctx, &cip.AssociateSoftwareTokenInput{Session: started.Session})
	if err != nil || aws.ToString(associated.SecretCode) == "" || associated.Session == nil {
		t.Fatalf("associate = %+v, %v", associated, err)
	}

	_, err = users.AssociateSoftwareToken(ctx, &cip.AssociateSoftwareTokenInput{Session: started.Session})
	code(t, err, "NotAuthorizedException")
	_, err = users.VerifySoftwareToken(
		ctx,
		&cip.VerifySoftwareTokenInput{Session: associated.Session, UserCode: new(wrongTOTP(t, *associated.SecretCode))},
	)
	code(t, err, "CodeMismatchException")
	verified, err := users.VerifySoftwareToken(ctx, &cip.VerifySoftwareTokenInput{
		Session:  associated.Session,
		UserCode: new(totpCode(t, *associated.SecretCode, time.Now())),
	})
	if err != nil || verified.Session == nil {
		t.Fatalf("verify = %+v, %v", verified, err)
	}

	_, err = users.VerifySoftwareToken(ctx, &cip.VerifySoftwareTokenInput{
		Session:  associated.Session,
		UserCode: new(totpCode(t, *associated.SecretCode, time.Now())),
	})
	code(t, err, "NotAuthorizedException")
	answer := &cip.RespondToAuthChallengeInput{
		ClientId: clientID, Session: verified.Session, ChallengeName: started.ChallengeName,
		ChallengeResponses: map[string]string{"USERNAME": "alice"},
	}
	signed, err := users.RespondToAuthChallenge(ctx, answer)
	if err != nil || signed.AuthenticationResult == nil {
		t.Fatalf("setup completion = %+v, %v", signed, err)
	}

	_, err = users.RespondToAuthChallenge(ctx, answer)
	code(t, err, "NotAuthorizedException")
	_, err = users.SetUserMFAPreference(ctx, &cip.SetUserMFAPreferenceInput{
		AccessToken:              signed.AuthenticationResult.AccessToken,
		SoftwareTokenMfaSettings: &ciptypes.SoftwareTokenMfaSettingsType{Enabled: false},
	})
	code(t, err, "InvalidParameterException")
	if next := startMFA(t, users, clientID, "alice"); next.ChallengeName != ciptypes.ChallengeNameTypeSoftwareTokenMfa {
		t.Fatalf("next = %+v", next)
	}
}

// Ensure expired and replaced pending TOTP secrets don't replace the active authenticator.
func TestCognitoTOTPAssociationBinding(t *testing.T) {
	store := &memory.Store{}
	s, stop := serverStore(t, testConfig(), &fakeRuntime{}, store)
	t.Cleanup(stop)
	ctx := context.Background()
	admin, users := cognitoClient(s, "test", "us-east-1"), anonymousCognito(s)
	pool := createPool(t, admin, "totp binding", nil)
	clientID := mfaClient(t, admin, pool.Id, "app")
	other := mfaClient(t, admin, pool.Id, "other")
	createMFAUser(t, admin, pool.Id, "alice")
	token := startMFA(t, users, clientID, "alice").AuthenticationResult.AccessToken
	otherToken := startMFA(t, users, other, "alice").AuthenticationResult.AccessToken
	_, err := users.AssociateSoftwareToken(ctx, &cip.AssociateSoftwareTokenInput{AccessToken: token})
	code(t, err, "SoftwareTokenMFANotFoundException")
	if _, err = admin.SetUserPoolMfaConfig(ctx, &cip.SetUserPoolMfaConfigInput{
		UserPoolId:                    pool.Id,
		SoftwareTokenMfaConfiguration: &ciptypes.SoftwareTokenMfaConfigType{Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}

	secret := setupTOTP(t, users, token)
	pending, err := users.AssociateSoftwareToken(ctx, &cip.AssociateSoftwareTokenInput{AccessToken: token})
	if err != nil {
		t.Fatal(err)
	}

	_, err = users.VerifySoftwareToken(
		ctx,
		&cip.VerifySoftwareTokenInput{
			AccessToken: otherToken,
			UserCode:    new(totpCode(t, *pending.SecretCode, time.Now())),
		},
	)
	code(t, err, "NotAuthorizedException")
	setCognitoTime(t, store, "user", *pool.Id, "alice", "PendingTOTPExpires", time.Now().Add(-time.Second))
	_, err = users.VerifySoftwareToken(
		ctx,
		&cip.VerifySoftwareTokenInput{AccessToken: token, UserCode: new(totpCode(t, *pending.SecretCode, time.Now()))},
	)
	code(t, err, "NotAuthorizedException")
	_, err = users.AssociateSoftwareToken(
		ctx,
		&cip.AssociateSoftwareTokenInput{AccessToken: token, Session: new("bogus")},
	)
	code(t, err, "InvalidParameterException")
	if _, err = admin.SetUserPoolMfaConfig(
		ctx,
		&cip.SetUserPoolMfaConfigInput{UserPoolId: pool.Id, MfaConfiguration: ciptypes.UserPoolMfaTypeOptional},
	); err != nil {
		t.Fatal(err)
	}

	challenge := startMFA(t, users, clientID, "alice")
	if _, err = users.RespondToAuthChallenge(ctx, &cip.RespondToAuthChallengeInput{
		ClientId:      clientID,
		Session:       challenge.Session,
		ChallengeName: challenge.ChallengeName,
		ChallengeResponses: map[string]string{
			"USERNAME":                "alice",
			"SOFTWARE_TOKEN_MFA_CODE": totpCode(t, secret, time.Now()),
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func wrongTOTP(t *testing.T, secret string) string {
	t.Helper()
	now := time.Now()
	valid := []string{
		totpCode(t, secret, now.Add(-30*time.Second)),
		totpCode(t, secret, now),
		totpCode(t, secret, now.Add(30*time.Second)),
	}
	for value := range 1_000_000 {
		candidate := fmt.Sprintf("%06d", value)
		if !slices.Contains(valid, candidate) {
			return candidate
		}
	}

	t.Fatal("no invalid TOTP available")
	return ""
}

func TestCognitoMFASessionConcurrentSingleUse(t *testing.T) {
	store := &memory.Store{}
	s, stop := serverStore(t, testConfig(), &fakeRuntime{}, store)
	t.Cleanup(stop)
	ctx := context.Background()
	admin, users := cognitoClient(s, "test", "us-east-1"), anonymousCognito(s)
	pool := createPool(t, admin, "concurrent MFA", &cip.CreateUserPoolInput{
		MfaConfiguration: ciptypes.UserPoolMfaTypeOn,
		SmsConfiguration: &ciptypes.SmsConfigurationType{SnsCallerArn: new("arn:aws:iam::000000000000:role/sms")},
	})
	clientID := mfaClient(t, admin, pool.Id, "app")
	createMFAUser(t, admin, pool.Id, "alice")
	if _, err := admin.UpdateUserPoolClient(ctx, &cip.UpdateUserPoolClientInput{
		UserPoolId: pool.Id, ClientId: clientID,
		AuthSessionValidity: new(int32(9)), ExplicitAuthFlows: []ciptypes.ExplicitAuthFlowsType{
			ciptypes.ExplicitAuthFlowsTypeAllowUserPasswordAuth,
		},
	}); err != nil {
		t.Fatal(err)
	}
	started := startMFA(t, users, clientID, "alice")
	if started.ChallengeName != ciptypes.ChallengeNameTypeSmsMfa {
		t.Fatalf("required SMS = %+v", started)
	}

	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	if err := store.View(ctx, func(tx storage.ReadTx) error {
		record, _, readErr := storage.Read[struct{ Expires time.Time }](
			tx,
			scope.Prefix("cognitoidp", "session")+*pool.Id+"/"+*started.Session,
		)
		if readErr != nil {
			return readErr
		}

		if remaining := time.Until(record.Expires); remaining < 8*time.Minute || remaining > 9*time.Minute {
			t.Fatalf("custom AuthSessionValidity remaining = %v", remaining)
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	_, _, value := cognitoMessage(t, s, "sms:+15555550123")
	answer := &cip.RespondToAuthChallengeInput{
		ClientId: clientID, Session: started.Session, ChallengeName: started.ChallengeName,
		ChallengeResponses: map[string]string{"USERNAME": "alice", "SMS_MFA_CODE": value},
	}
	const attempts = 8
	errors := make(chan error, attempts)
	gate := make(chan struct{})
	for range attempts {
		go func() {
			<-gate
			_, err := users.RespondToAuthChallenge(ctx, answer)
			errors <- err
		}()
	}

	close(gate)
	successes := 0
	for range attempts {
		if err := <-errors; err == nil {
			successes++
		} else {
			code(t, err, "NotAuthorizedException")
		}
	}

	if successes != 1 {
		t.Fatalf("successful concurrent answers = %d", successes)
	}
}
