package integration_test

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cip "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	ciptypes "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"

	"github.com/amjadjibon/tinystack/internal/config"
)

// anonymousCognito calls Cognito unsigned, as an app's users do.
func anonymousCognito(s *httptest.Server) *cip.Client {
	return cip.New(cip.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(s.URL), Credentials: aws.AnonymousCredentials{},
		Retryer: aws.NopRetryer{},
	})
}

func hashFor(clientID, secret, username string) *string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(username + clientID))
	return aws.String(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
}

// verifiedClaims checks a token's signature with the key its issuer's JWKS
// publishes and returns its claims.
func verifiedClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token = %q", token)
	}

	var claims map[string]any
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payload, &claims) != nil {
		t.Fatalf("claims: %v", err)
	}

	resp, err := http.Get(claims["iss"].(string) + "/.well-known/jwks.json") // #nosec G107 -- the test server.
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()
	var set struct {
		Keys []struct{ Kid, N, E, Alg string } `json:"keys"`
	}

	if err = json.NewDecoder(resp.Body).Decode(&set); err != nil || len(set.Keys) != 1 || set.Keys[0].Alg != "RS256" {
		t.Fatalf("jwks = %+v, %v", set, err)
	}

	n, _ := base64.RawURLEncoding.DecodeString(set.Keys[0].N)
	e, _ := base64.RawURLEncoding.DecodeString(set.Keys[0].E)
	key := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err = rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("signature: %v", err)
	}

	return claims
}

func TestCognitoSignUpAndSignIn(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.SecretKeys = map[string]string{"test": "test"}
	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx := context.Background()
	admin := cognitoClient(s, "test", "us-east-1")
	users := anonymousCognito(s)

	// Unsigned calls reach only the operations users make.
	_, err := users.ListUserPools(ctx, &cip.ListUserPoolsInput{MaxResults: aws.Int32(1)})
	code(t, err, "MissingAuthenticationTokenException")

	pool := createPool(t, admin, "sign-up", &cip.CreateUserPoolInput{
		UsernameAttributes:     []ciptypes.UsernameAttributeType{ciptypes.UsernameAttributeTypeEmail},
		AutoVerifiedAttributes: []ciptypes.VerifiedAttributeType{ciptypes.VerifiedAttributeTypeEmail},
		Schema: []ciptypes.SchemaAttributeType{
			{Name: aws.String("plan"), AttributeDataType: ciptypes.AttributeDataTypeString},
		},
	})
	created, err := admin.CreateUserPoolClient(ctx, &cip.CreateUserPoolClientInput{
		UserPoolId: pool.Id, ClientName: aws.String("app"), GenerateSecret: true,
		ExplicitAuthFlows: []ciptypes.ExplicitAuthFlowsType{
			ciptypes.ExplicitAuthFlowsTypeAllowUserPasswordAuth, ciptypes.ExplicitAuthFlowsTypeAllowRefreshTokenAuth,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	clientID, secret := created.UserPoolClient.ClientId, aws.ToString(created.UserPoolClient.ClientSecret)
	email := "eve@example.com"
	hash := hashFor(*clientID, secret, email)
	_, err = users.SignUp(ctx, &cip.SignUpInput{
		ClientId: clientID, Username: aws.String(email), Password: aws.String("Sup3r-Secret"),
	})
	code(t, err, "NotAuthorizedException")

	signedUp, err := users.SignUp(ctx, &cip.SignUpInput{
		ClientId: clientID, SecretHash: hash, Username: aws.String(email), Password: aws.String("Sup3r-Secret"),
		UserAttributes: attrs("custom:plan", "pro"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if signedUp.UserConfirmed || len(aws.ToString(signedUp.UserSub)) != 36 ||
		aws.ToString(signedUp.CodeDeliveryDetails.Destination) != "e***@e***" {
		t.Fatalf("sign-up = %+v %+v", signedUp, signedUp.CodeDeliveryDetails)
	}

	_, err = users.SignUp(ctx, &cip.SignUpInput{
		ClientId: clientID, SecretHash: hash, Username: aws.String(email), Password: aws.String("Sup3r-Secret"),
	})
	code(t, err, "UsernameExistsException")

	passwordAuth := func(password string) (*cip.InitiateAuthOutput, error) {
		return users.InitiateAuth(ctx, &cip.InitiateAuthInput{
			ClientId: clientID, AuthFlow: ciptypes.AuthFlowTypeUserPasswordAuth,
			AuthParameters: map[string]string{"USERNAME": email, "PASSWORD": password, "SECRET_HASH": *hash},
		})
	}

	_, err = passwordAuth("Sup3r-Secret")
	code(t, err, "UserNotConfirmedException")

	_, err = users.ConfirmSignUp(ctx, &cip.ConfirmSignUpInput{
		ClientId: clientID, SecretHash: hash, Username: aws.String(email), ConfirmationCode: aws.String("000000x"),
	})
	code(t, err, "CodeMismatchException")

	_, _, confirmation := cognitoMessage(t, s, "email:"+email)
	if _, err = users.ConfirmSignUp(ctx, &cip.ConfirmSignUpInput{
		ClientId: clientID, SecretHash: hash, Username: aws.String(email), ConfirmationCode: aws.String(confirmation),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = passwordAuth("wrong")
	code(t, err, "NotAuthorizedException")

	signedIn, err := passwordAuth("Sup3r-Secret")
	if err != nil {
		t.Fatal(err)
	}

	result := signedIn.AuthenticationResult
	id := verifiedClaims(t, aws.ToString(result.IdToken))
	if id["token_use"] != "id" || id["email"] != email || id["email_verified"] != true || id["custom:plan"] != "pro" ||
		id["aud"] != *clientID || id["iss"] != s.URL+"/"+aws.ToString(pool.Id) ||
		id["sub"] != aws.ToString(signedUp.UserSub) || result.ExpiresIn != 3600 {
		t.Fatalf("id token = %+v", id)
	}

	access := verifiedClaims(t, aws.ToString(result.AccessToken))
	if access["token_use"] != "access" || access["client_id"] != *clientID ||
		access["scope"] != "aws.cognito.signin.user.admin" || access["origin_jti"] != id["origin_jti"] {
		t.Fatalf("access token = %+v", access)
	}

	// A signed call to an operation users make is verified, then served.
	signedDoc, err := admin.GetUser(ctx, &cip.GetUserInput{AccessToken: result.AccessToken})
	if err != nil || aws.ToString(signedDoc.Username) != aws.ToString(signedUp.UserSub) {
		t.Fatalf("signed get = %+v, %v", signedDoc, err)
	}

	me, err := users.GetUser(ctx, &cip.GetUserInput{AccessToken: result.AccessToken})
	if err != nil || attribute(me.UserAttributes, "email") != email {
		t.Fatalf("user = %+v, %v", me, err)
	}

	updated, err := users.UpdateUserAttributes(ctx, &cip.UpdateUserAttributesInput{
		AccessToken: result.AccessToken, UserAttributes: attrs("name", "Eve", "email", "eve@example.org"),
	})
	if err != nil || len(updated.CodeDeliveryDetailsList) != 1 {
		t.Fatalf("updated = %+v, %v", updated, err)
	}

	_, err = users.UpdateUserAttributes(ctx, &cip.UpdateUserAttributesInput{
		AccessToken: result.AccessToken, UserAttributes: attrs("email_verified", "true"),
	})
	code(t, err, "NotAuthorizedException")

	_, _, verification := cognitoMessage(t, s, "email:eve@example.org")
	if _, err = users.VerifyUserAttribute(ctx, &cip.VerifyUserAttributeInput{
		AccessToken: result.AccessToken, AttributeName: aws.String("email"), Code: aws.String(verification),
	}); err != nil {
		t.Fatal(err)
	}

	email, hash = "eve@example.org", hashFor(*clientID, secret, "eve@example.org")
	_, err = users.ChangePassword(ctx, &cip.ChangePasswordInput{
		AccessToken:      result.AccessToken,
		PreviousPassword: aws.String("wrong"),
		ProposedPassword: aws.String("N3w-Secret"),
	})
	code(t, err, "NotAuthorizedException")

	if _, err = users.ChangePassword(ctx, &cip.ChangePasswordInput{
		AccessToken: result.AccessToken, PreviousPassword: aws.String("Sup3r-Secret"),
		ProposedPassword: aws.String("N3w-Secret"),
	}); err != nil {
		t.Fatal(err)
	}

	refreshed, err := users.InitiateAuth(ctx, &cip.InitiateAuthInput{
		ClientId: clientID, AuthFlow: ciptypes.AuthFlowTypeRefreshTokenAuth,
		AuthParameters: map[string]string{
			"REFRESH_TOKEN": aws.ToString(result.RefreshToken),
			"SECRET_HASH":   *hashFor(*clientID, secret, aws.ToString(signedUp.UserSub)),
		},
	})
	if err != nil || refreshed.AuthenticationResult.RefreshToken != nil {
		t.Fatalf("refreshed = %+v, %v", refreshed, err)
	}

	if claims := verifiedClaims(t, aws.ToString(refreshed.AuthenticationResult.IdToken)); claims["name"] != "Eve" {
		t.Fatalf("refreshed id token = %+v", claims)
	}

	// Signing out everywhere revokes both tokens.
	if _, err = users.GlobalSignOut(ctx, &cip.GlobalSignOutInput{AccessToken: result.AccessToken}); err != nil {
		t.Fatal(err)
	}

	_, err = users.GetUser(ctx, &cip.GetUserInput{AccessToken: refreshed.AuthenticationResult.AccessToken})
	code(t, err, "NotAuthorizedException")

	_, err = users.InitiateAuth(ctx, &cip.InitiateAuthInput{
		ClientId: clientID, AuthFlow: ciptypes.AuthFlowTypeRefreshTokenAuth,
		AuthParameters: map[string]string{"REFRESH_TOKEN": aws.ToString(result.RefreshToken), "SECRET_HASH": *hash},
	})
	code(t, err, "NotAuthorizedException")

	_, err = users.GetUser(ctx, &cip.GetUserInput{AccessToken: aws.String("not.a.token")})
	code(t, err, "NotAuthorizedException")

	// Forgotten passwords are reset with the code sent to the verified address.
	forgot, err := users.ForgotPassword(ctx, &cip.ForgotPasswordInput{
		ClientId: clientID, SecretHash: hash, Username: aws.String(email),
	})
	if err != nil || forgot.CodeDeliveryDetails.DeliveryMedium != ciptypes.DeliveryMediumTypeEmail {
		t.Fatalf("forgot = %+v, %v", forgot, err)
	}

	_, _, reset := cognitoMessage(t, s, "email:"+email)
	if _, err = users.ConfirmForgotPassword(ctx, &cip.ConfirmForgotPasswordInput{
		ClientId: clientID, SecretHash: hash, Username: aws.String(email), ConfirmationCode: aws.String(reset),
		Password: aws.String("Th1rd-Secret"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = users.ConfirmForgotPassword(ctx, &cip.ConfirmForgotPasswordInput{
		ClientId: clientID, SecretHash: hash, Username: aws.String(email), ConfirmationCode: aws.String(reset),
		Password: aws.String("Fourth-Secret1"),
	})
	code(t, err, "CodeMismatchException")

	signedIn, err = passwordAuth("Th1rd-Secret")
	if err != nil {
		t.Fatal(err)
	}

	if _, err = users.DeleteUser(ctx, &cip.DeleteUserInput{
		AccessToken: signedIn.AuthenticationResult.AccessToken,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = passwordAuth("Th1rd-Secret")
	code(t, err, "UserNotFoundException")

	_, err = users.InitiateAuth(ctx, &cip.InitiateAuthInput{
		ClientId: aws.String("missing"), AuthFlow: ciptypes.AuthFlowTypeUserPasswordAuth,
	})
	code(t, err, "ResourceNotFoundException")
}

func TestCognitoAdminAuth(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	admin := cognitoClient(s, "test", "us-east-1")
	users := anonymousCognito(s)
	pool := createPool(t, admin, "admin auth", &cip.CreateUserPoolInput{
		AdminCreateUserConfig: &ciptypes.AdminCreateUserConfigType{AllowAdminCreateUserOnly: true},
		Policies: &ciptypes.UserPoolPolicyType{PasswordPolicy: &ciptypes.PasswordPolicyType{
			MinimumLength: aws.Int32(8), PasswordHistorySize: aws.Int32(2),
		}},
		Schema: []ciptypes.SchemaAttributeType{{Name: aws.String("name"), Required: aws.Bool(true)}},
	})
	created, err := admin.CreateUserPoolClient(ctx, &cip.CreateUserPoolClientInput{
		UserPoolId:                 pool.Id,
		ClientName:                 aws.String("backend"),
		PreventUserExistenceErrors: ciptypes.PreventUserExistenceErrorTypesEnabled,
		ExplicitAuthFlows: []ciptypes.ExplicitAuthFlowsType{
			ciptypes.ExplicitAuthFlowsTypeAllowAdminUserPasswordAuth,
			ciptypes.ExplicitAuthFlowsTypeAllowRefreshTokenAuth,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	clientID := created.UserPoolClient.ClientId
	_, err = users.SignUp(ctx, &cip.SignUpInput{
		ClientId: clientID, Username: aws.String("walk-in"), Password: aws.String("Passw0rd!"),
	})
	code(t, err, "NotAuthorizedException")

	if _, err = admin.AdminCreateUser(ctx, &cip.AdminCreateUserInput{
		UserPoolId: pool.Id, Username: aws.String("fay"), TemporaryPassword: aws.String("Temp0rary!"),
		MessageAction: ciptypes.MessageActionTypeSuppress,
	}); err == nil {
		t.Fatal("created a user without a required attribute")
	}

	if _, err = admin.AdminCreateUser(ctx, &cip.AdminCreateUserInput{
		UserPoolId: pool.Id, Username: aws.String("fay"), TemporaryPassword: aws.String("Temp0rary!"),
		MessageAction: ciptypes.MessageActionTypeSuppress, UserAttributes: attrs("name", "Fay"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = admin.CreateGroup(ctx, &cip.CreateGroupInput{
		UserPoolId: pool.Id, GroupName: aws.String("staff"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = admin.AdminAddUserToGroup(ctx, &cip.AdminAddUserToGroupInput{
		UserPoolId: pool.Id, Username: aws.String("fay"), GroupName: aws.String("staff"),
	}); err != nil {
		t.Fatal(err)
	}

	adminAuth := func(user, password string) (*cip.AdminInitiateAuthOutput, error) {
		return admin.AdminInitiateAuth(ctx, &cip.AdminInitiateAuthInput{
			UserPoolId: pool.Id, ClientId: clientID, AuthFlow: ciptypes.AuthFlowTypeAdminUserPasswordAuth,
			AuthParameters: map[string]string{"USERNAME": user, "PASSWORD": password},
		})
	}

	// Unknown users look like wrong passwords to this client.
	_, err = adminAuth("nobody", "Temp0rary!")
	code(t, err, "NotAuthorizedException")

	_, err = users.InitiateAuth(ctx, &cip.InitiateAuthInput{
		ClientId: clientID, AuthFlow: ciptypes.AuthFlowTypeUserPasswordAuth,
		AuthParameters: map[string]string{"USERNAME": "fay", "PASSWORD": "Temp0rary!"},
	})
	code(t, err, "InvalidParameterException")

	// This client does not allow SRP.
	_, err = users.InitiateAuth(ctx, &cip.InitiateAuthInput{
		ClientId: clientID, AuthFlow: ciptypes.AuthFlowTypeUserSrpAuth,
		AuthParameters: map[string]string{"USERNAME": "fay", "SRP_A": "1"},
	})
	code(t, err, "InvalidParameterException")

	challenge, err := adminAuth("fay", "Temp0rary!")
	if err != nil || challenge.ChallengeName != ciptypes.ChallengeNameTypeNewPasswordRequired ||
		challenge.AuthenticationResult != nil || challenge.ChallengeParameters["USER_ID_FOR_SRP"] != "fay" {
		t.Fatalf("challenge = %+v, %v", challenge, err)
	}

	_, err = admin.AdminRespondToAuthChallenge(ctx, &cip.AdminRespondToAuthChallengeInput{
		UserPoolId: pool.Id, ClientId: clientID, ChallengeName: ciptypes.ChallengeNameTypeNewPasswordRequired,
		Session: aws.String("forged"), ChallengeResponses: map[string]string{"USERNAME": "fay", "NEW_PASSWORD": "x"},
	})
	code(t, err, "NotAuthorizedException")

	_, err = admin.AdminRespondToAuthChallenge(ctx, &cip.AdminRespondToAuthChallengeInput{
		UserPoolId: pool.Id, ClientId: clientID, ChallengeName: ciptypes.ChallengeNameTypeNewPasswordRequired,
		Session: challenge.Session, ChallengeResponses: map[string]string{"USERNAME": "fay", "NEW_PASSWORD": "weak"},
	})
	code(t, err, "InvalidPasswordException")

	answered, err := admin.AdminRespondToAuthChallenge(ctx, &cip.AdminRespondToAuthChallengeInput{
		UserPoolId: pool.Id, ClientId: clientID, ChallengeName: ciptypes.ChallengeNameTypeNewPasswordRequired,
		Session: challenge.Session, ChallengeResponses: map[string]string{
			"USERNAME": "fay", "NEW_PASSWORD": "Perm4nent!", "userAttributes.nickname": "F",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result := answered.AuthenticationResult
	id := verifiedClaims(t, aws.ToString(result.IdToken))
	if groups, _ := id["cognito:groups"].([]any); len(groups) != 1 || groups[0] != "staff" ||
		id["cognito:username"] != "fay" || id["nickname"] != "F" {
		t.Fatalf("id token = %+v", id)
	}

	got, err := admin.AdminGetUser(ctx, &cip.AdminGetUserInput{UserPoolId: pool.Id, Username: aws.String("fay")})
	if err != nil || got.UserStatus != ciptypes.UserStatusTypeConfirmed {
		t.Fatalf("user = %+v, %v", got, err)
	}

	// The password history covers the current and the temporary password.
	for _, reused := range []string{"Perm4nent!", "Temp0rary!"} {
		_, err = admin.AdminSetUserPassword(ctx, &cip.AdminSetUserPasswordInput{
			UserPoolId: pool.Id, Username: aws.String("fay"), Password: aws.String(reused), Permanent: true,
		})
		code(t, err, "PasswordHistoryPolicyViolationException")
	}

	// Revoking a refresh token ends its session only.
	second, err := adminAuth("fay", "Perm4nent!")
	if err != nil {
		t.Fatal(err)
	}

	if _, err = users.RevokeToken(ctx, &cip.RevokeTokenInput{
		ClientId: clientID, Token: result.RefreshToken,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = users.GetUser(ctx, &cip.GetUserInput{AccessToken: result.AccessToken})
	code(t, err, "NotAuthorizedException")

	if _, err = users.GetUser(ctx, &cip.GetUserInput{
		AccessToken: second.AuthenticationResult.AccessToken,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = users.RevokeToken(ctx, &cip.RevokeTokenInput{ClientId: clientID, Token: result.AccessToken})
	code(t, err, "UnsupportedTokenTypeException")

	if _, err = admin.AdminUserGlobalSignOut(ctx, &cip.AdminUserGlobalSignOutInput{
		UserPoolId: pool.Id, Username: aws.String("fay"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = users.GetUser(ctx, &cip.GetUserInput{AccessToken: second.AuthenticationResult.AccessToken})
	code(t, err, "NotAuthorizedException")

	if _, err = admin.AdminDisableUser(ctx, &cip.AdminDisableUserInput{
		UserPoolId: pool.Id, Username: aws.String("fay"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = adminAuth("fay", "Perm4nent!")
	code(t, err, "NotAuthorizedException")

	// The pool's discovery document names its key set.
	var discovery struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}

	path := "/" + aws.ToString(pool.Id) + "/.well-known/openid-configuration"
	if status := internal(t, s, http.MethodGet, path, &discovery); status != http.StatusOK ||
		discovery.Issuer != s.URL+"/"+aws.ToString(
			pool.Id,
		) || discovery.JWKSURI != discovery.Issuer+"/.well-known/jwks.json" {
		t.Fatalf("discovery = %d %+v", status, discovery)
	}

	if status := internal(
		t,
		s,
		http.MethodGet,
		"/us-east-1_missing/.well-known/jwks.json",
		nil,
	); status != http.StatusNotFound {
		t.Fatalf("missing pool jwks = %d", status)
	}
}

// srpClient is the client half of Cognito's SRP, written as the AWS client
// libraries compute it.
type srpClient struct {
	a, bigA *big.Int
	pool    string
}

var (
	srpGroup, _ = new(
		big.Int,
	).SetString("FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B2"+
		"2514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F4"+
		"06B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3DC2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F8365"+
		"5D23DCA3AD961C62F356208552BB9ED529077096966D670C354E4ABC9804F1746C08CA18217C32905E462E36CE3BE39E772C180E86"+
		"039B2783A2EC07A28FB5C55DF06F4C52C9DE2BCBF6955817183995497CEA956AE515D2261898FA051015728E5A8AAAC42DAD33170D"+
		"04507A33A85521ABDF1CBA64ECFB850458DBEF0A8AEA71575D060C7DB3970F85A6E1E4C7ABF5AE8CDB0933D71E8C94E04A25619DCE"+
		"E3D2261AD2EE6BF12FFA06D98A0864D87602733EC86A64521F2B18177B200CBBE117577A615D6C770988C0BAD946E208E24FA074E5"+
		"AB3143DB5BFCE0FD108E4B82D120A93AD2CAFFFFFFFFFFFFFFFF", 16)
	srpTwo = big.NewInt(2)
)

func srpPad(h string) string {
	switch {
	case len(h)%2 == 1:
		return "0" + h
	case strings.ContainsRune("89abcdef", rune(h[0])):
		return "00" + h
	}

	return h
}

func srpHash(h string) *big.Int {
	b, _ := hex.DecodeString(h)
	sum := sha256.Sum256(b)
	return new(big.Int).SetBytes(sum[:])
}

func newSRPClient(pool string) *srpClient {
	a, _ := rand.Int(rand.Reader, srpGroup)
	return &srpClient{a: a, bigA: new(big.Int).Exp(srpTwo, a, srpGroup), pool: pool}
}

// claim answers a PASSWORD_VERIFIER challenge for password.
func (c *srpClient) claim(params map[string]string, password string) map[string]string {
	userID, salt := params["USER_ID_FOR_SRP"], params["SALT"]
	bigB, _ := new(big.Int).SetString(params["SRP_B"], 16)
	k := srpHash(srpPad(srpGroup.Text(16)) + srpPad(srpTwo.Text(16)))
	u := srpHash(srpPad(c.bigA.Text(16)) + srpPad(bigB.Text(16)))
	_, name, _ := strings.Cut(c.pool, "_")
	inner := sha256.Sum256([]byte(name + userID + ":" + password))
	x := srpHash(srpPad(salt) + hex.EncodeToString(inner[:]))
	// S = (B - k·g^x)^(a + u·x)
	base := new(big.Int).Sub(bigB, new(big.Int).Mul(k, new(big.Int).Exp(srpTwo, x, srpGroup)))
	base.Mod(base, srpGroup)
	exp := new(big.Int).Add(c.a, new(big.Int).Mul(u, x))
	s := new(big.Int).Exp(base, exp, srpGroup)
	ikm, _ := hex.DecodeString(srpPad(s.Text(16)))
	hkdfSalt, _ := hex.DecodeString(srpPad(u.Text(16)))
	prk := hmac.New(sha256.New, hkdfSalt)
	prk.Write(ikm)
	okm := hmac.New(sha256.New, prk.Sum(nil))
	okm.Write([]byte("Caldera Derived Key\x01"))
	block, _ := base64.StdEncoding.DecodeString(params["SECRET_BLOCK"])
	timestamp := "Sat Sep 7 04:05:06 UTC 2026"
	mac := hmac.New(sha256.New, okm.Sum(nil)[:16])
	mac.Write([]byte(name + userID))
	mac.Write(block)
	mac.Write([]byte(timestamp))
	return map[string]string{
		"USERNAME": userID, "PASSWORD_CLAIM_SECRET_BLOCK": params["SECRET_BLOCK"], "TIMESTAMP": timestamp,
		"PASSWORD_CLAIM_SIGNATURE": base64.StdEncoding.EncodeToString(mac.Sum(nil)),
	}
}

func TestCognitoSRP(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	admin := cognitoClient(s, "test", "us-east-1")
	users := anonymousCognito(s)
	pool := createPool(t, admin, "srp", &cip.CreateUserPoolInput{
		UsernameAttributes: []ciptypes.UsernameAttributeType{ciptypes.UsernameAttributeTypeEmail},
	})
	poolID := aws.ToString(pool.Id)

	// A client without ExplicitAuthFlows allows SRP and refreshing.
	created, err := admin.CreateUserPoolClient(ctx, &cip.CreateUserPoolClientInput{
		UserPoolId: pool.Id, ClientName: aws.String("spa"),
	})
	if err != nil {
		t.Fatal(err)
	}

	clientID := created.UserPoolClient.ClientId
	if _, err = admin.AdminCreateUser(ctx, &cip.AdminCreateUserInput{
		UserPoolId: pool.Id, Username: aws.String("gus@example.com"), TemporaryPassword: aws.String("Temp0rary!"),
		MessageAction: ciptypes.MessageActionTypeSuppress,
	}); err != nil {
		t.Fatal(err)
	}

	srpSignIn := func(password string) (*cip.RespondToAuthChallengeOutput, error) {
		client := newSRPClient(poolID)
		started, startErr := users.InitiateAuth(ctx, &cip.InitiateAuthInput{
			ClientId: clientID, AuthFlow: ciptypes.AuthFlowTypeUserSrpAuth,
			AuthParameters: map[string]string{"USERNAME": "gus@example.com", "SRP_A": client.bigA.Text(16)},
		})
		if startErr != nil {
			t.Fatal(startErr)
		}

		if started.ChallengeName != ciptypes.ChallengeNameTypePasswordVerifier ||
			started.ChallengeParameters["USER_ID_FOR_SRP"] == "gus@example.com" {
			t.Fatalf("challenge = %+v", started)
		}

		return users.RespondToAuthChallenge(ctx, &cip.RespondToAuthChallengeInput{
			ClientId: clientID, ChallengeName: ciptypes.ChallengeNameTypePasswordVerifier,
			ChallengeResponses: client.claim(started.ChallengeParameters, password),
		})
	}

	_, err = srpSignIn("Wrong-Passw0rd")
	code(t, err, "NotAuthorizedException")

	// The temporary password must be replaced first.
	answered, err := srpSignIn("Temp0rary!")
	if err != nil || answered.ChallengeName != ciptypes.ChallengeNameTypeNewPasswordRequired {
		t.Fatalf("answered = %+v, %v", answered, err)
	}

	if _, err = users.RespondToAuthChallenge(ctx, &cip.RespondToAuthChallengeInput{
		ClientId: clientID, ChallengeName: ciptypes.ChallengeNameTypeNewPasswordRequired, Session: answered.Session,
		ChallengeResponses: map[string]string{"USERNAME": "gus@example.com", "NEW_PASSWORD": "Perm4nent!"},
	}); err != nil {
		t.Fatal(err)
	}

	signedIn, err := srpSignIn("Perm4nent!")
	if err != nil || signedIn.AuthenticationResult == nil || signedIn.AuthenticationResult.RefreshToken == nil {
		t.Fatalf("signed in = %+v, %v", signedIn, err)
	}

	if claims := verifiedClaims(
		t,
		aws.ToString(signedIn.AuthenticationResult.IdToken),
	); claims["email"] != "gus@example.com" {
		t.Fatalf("id token = %+v", claims)
	}

	_, err = users.RespondToAuthChallenge(ctx, &cip.RespondToAuthChallengeInput{
		ClientId: clientID, ChallengeName: ciptypes.ChallengeNameTypePasswordVerifier,
		ChallengeResponses: map[string]string{"USERNAME": "gus@example.com", "PASSWORD_CLAIM_SECRET_BLOCK": "AAAA"},
	})
	code(t, err, "NotAuthorizedException")
}
