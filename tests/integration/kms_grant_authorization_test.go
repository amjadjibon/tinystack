package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/gateway"
)

func TestKMSGrantAuthorization(t *testing.T) {
	for _, cross := range []bool{false, true} {
		t.Run(fmt.Sprintf("cross=%t", cross), func(t *testing.T) {
			cfg := testConfig()
			cfg.Identity.Accounts["test"] = cfg.Defaults.Account
			cfg.Identity.Accounts["other"] = "222222222222"
			cfg.Identity.Authorization = config.AuthorizationEnforce
			s := serverConfig(t, cfg, &fakeRuntime{})
			ctx := context.Background()
			issued := authorizationUser(t, s)
			owner := "test"
			if cross {
				owner = "other"
			}

			root := kmsClient(s, owner, "us-east-1")
			user := kmsClient(s, *issued.AccessKeyId, "us-east-1")
			key := createKey(t, root, "grants")
			grantee := "arn:aws:iam::" + cfg.Defaults.Account + ":user/apps/alice"
			created, err := root.CreateGrant(ctx, &kms.CreateGrantInput{
				KeyId: key.Arn, GranteePrincipal: &grantee,
				Operations: []kmstypes.GrantOperation{"Encrypt", "RetireGrant"},
			})
			mustAuthorize(t, err)
			encrypt := func(tokens []string) error {
				_, callErr := user.Encrypt(ctx, &kms.EncryptInput{
					KeyId: key.Arn, Plaintext: []byte("grant"), GrantTokens: tokens,
				})
				return callErr
			}

			mustAuthorize(t, encrypt(nil)) // no identity Allow, including cross-account
			mustAuthorize(t, encrypt([]string{*created.GrantToken}))
			mustAuthorize(t, encrypt([]string{"opaque-token"})) // immediate consistency; shape only
			_, err = user.DescribeKey(ctx, &kms.DescribeKeyInput{KeyId: key.Arn})
			wantCode(t, err, "AccessDeniedException")
			_, err = user.RevokeGrant(ctx, &kms.RevokeGrantInput{KeyId: key.Arn, GrantId: created.GrantId})
			wantCode(t, err, "AccessDeniedException")

			response, err := http.Get(s.URL + "/_tinystack/requests")
			mustAuthorize(t, err)
			var records struct {
				Requests []gateway.Record `json:"requests"`
			}
			mustAuthorize(t, json.NewDecoder(response.Body).Decode(&records))
			response.Body.Close()
			found := false
			for _, record := range records.Requests {
				if record.Operation == "Encrypt" && record.Authorization != nil && record.Authorization.Allowed {
					for _, check := range record.Authorization.Checks {
						found = found || (check.Grant != nil && check.Grant.ID == *created.GrantId)
					}
				}
			}

			if !found {
				t.Fatal("missing granting ID in inspector")
			}

			authorizationPolicy(t, s,
				`{"Statement":{"Effect":"Deny","Action":"kms:Encrypt","Resource":"*"}}`)
			wantCode(t, encrypt(nil), "AccessDeniedException")
			authorizationPolicy(t, s,
				`{"Statement":{"Effect":"Allow","Action":"iam:GetUser","Resource":"*"}}`)
			deny := fmt.Sprintf(`{"Statement":[{"Effect":"Allow","Principal":{"AWS":%q},`+
				`"Action":"kms:*","Resource":"*"},{"Effect":"Deny","Principal":"*",`+
				`"Action":"kms:Encrypt","Resource":"*"}]}`, "arn:aws:iam::"+*key.AWSAccountId+":root")
			_, err = root.PutKeyPolicy(ctx, &kms.PutKeyPolicyInput{KeyId: key.Arn, Policy: &deny})
			mustAuthorize(t, err)
			wantCode(t, encrypt(nil), "AccessDeniedException")
			allow := fmt.Sprintf(`{"Statement":{"Effect":"Allow","Principal":{"AWS":%q},`+
				`"Action":"kms:*","Resource":"*"}}`, "arn:aws:iam::"+*key.AWSAccountId+":root")
			_, err = root.PutKeyPolicy(ctx, &kms.PutKeyPolicyInput{KeyId: key.Arn, Policy: &allow})
			mustAuthorize(t, err)
			_, err = user.RetireGrant(ctx, &kms.RetireGrantInput{GrantToken: created.GrantToken})
			mustAuthorize(t, err)
			wantCode(t, encrypt(nil), "AccessDeniedException")

			created, err = root.CreateGrant(ctx, &kms.CreateGrantInput{
				KeyId: key.Arn, GranteePrincipal: &grantee, RetiringPrincipal: &grantee,
				Operations: []kmstypes.GrantOperation{"Encrypt"},
			})
			mustAuthorize(t, err)
			_, err = user.RetireGrant(ctx, &kms.RetireGrantInput{KeyId: key.Arn, GrantId: created.GrantId})
			mustAuthorize(t, err)
			wantCode(t, encrypt(nil), "AccessDeniedException")
			created, err = root.CreateGrant(ctx, &kms.CreateGrantInput{
				KeyId: key.Arn, GranteePrincipal: &grantee, Operations: []kmstypes.GrantOperation{"Encrypt"},
			})
			mustAuthorize(t, err)
			_, err = user.RetireGrant(ctx, &kms.RetireGrantInput{KeyId: key.Arn, GrantId: created.GrantId})
			wantCode(t, err, "AccessDeniedException")
			_, err = root.RevokeGrant(ctx, &kms.RevokeGrantInput{KeyId: key.Arn, GrantId: created.GrantId})
			mustAuthorize(t, err)
			wantCode(t, encrypt(nil), "AccessDeniedException")
		})
	}
}

func TestKMSGrantConstraintsAndDelegation(t *testing.T) {
	for _, equals := range []bool{false, true} {
		t.Run(fmt.Sprintf("equals=%t", equals), func(t *testing.T) {
			s := authorizationServer(t, config.AuthorizationEnforce, false)
			ctx := context.Background()
			issued := authorizationUser(t, s)
			root := kmsClient(s, "test", "us-east-1")
			user := kmsClient(s, *issued.AccessKeyId, "us-east-1")
			key := createKey(t, root, "constraints")
			grantee := "arn:aws:iam::" + testConfig().Defaults.Account + ":user/apps/alice"
			constraint := &kmstypes.GrantConstraints{EncryptionContextSubset: map[string]string{"app": "web"}}
			if equals {
				constraint = &kmstypes.GrantConstraints{EncryptionContextEquals: map[string]string{"app": "web"}}
			}

			parent, err := root.CreateGrant(ctx, &kms.CreateGrantInput{
				KeyId: key.KeyId, GranteePrincipal: &grantee, Constraints: constraint,
				Operations: []kmstypes.GrantOperation{
					"Encrypt", "Decrypt", "CreateGrant", "DescribeKey", "RetireGrant",
					"GenerateDataKey", "ReEncryptFrom", "ReEncryptTo",
				},
			})
			mustAuthorize(t, err)
			for _, contextValues := range []map[string]string{
				{"app": "web"}, {"app": "web", "extra": "value"}, {"app": "other"}, nil,
			} {
				_, callErr := user.Encrypt(ctx, &kms.EncryptInput{
					KeyId: key.KeyId, Plaintext: []byte("secret"), EncryptionContext: contextValues,
				})
				allowed := contextValues["app"] == "web" && (!equals || len(contextValues) == 1)
				if allowed {
					mustAuthorize(t, callErr)
				} else {
					wantCode(t, callErr, "AccessDeniedException")
				}
			}

			sealed, err := root.Encrypt(ctx, &kms.EncryptInput{
				KeyId: key.KeyId, Plaintext: []byte("secret"), EncryptionContext: map[string]string{"app": "web"},
			})
			mustAuthorize(t, err)
			_, err = user.Decrypt(ctx, &kms.DecryptInput{
				CiphertextBlob: sealed.CiphertextBlob, EncryptionContext: map[string]string{"app": "web"},
			})
			mustAuthorize(t, err)
			_, err = user.Decrypt(ctx, &kms.DecryptInput{CiphertextBlob: sealed.CiphertextBlob})
			wantCode(t, err, "AccessDeniedException")
			_, err = user.GenerateDataKey(ctx, &kms.GenerateDataKeyInput{
				KeyId:             key.KeyId,
				KeySpec:           kmstypes.DataKeySpecAes256,
				EncryptionContext: map[string]string{"app": "web"},
			})
			mustAuthorize(t, err)
			_, err = user.GenerateDataKey(ctx, &kms.GenerateDataKeyInput{
				KeyId: key.KeyId, KeySpec: kmstypes.DataKeySpecAes256,
			})
			wantCode(t, err, "AccessDeniedException")
			_, err = user.ReEncrypt(ctx, &kms.ReEncryptInput{
				CiphertextBlob: sealed.CiphertextBlob, DestinationKeyId: key.KeyId,
				SourceEncryptionContext:      map[string]string{"app": "web"},
				DestinationEncryptionContext: map[string]string{"app": "web"},
			})
			mustAuthorize(t, err)
			_, err = user.ReEncrypt(ctx, &kms.ReEncryptInput{
				CiphertextBlob: sealed.CiphertextBlob, DestinationKeyId: key.KeyId,
				SourceEncryptionContext: map[string]string{"app": "web"},
			})
			wantCode(t, err, "AccessDeniedException")
			_, err = user.DescribeKey(
				ctx,
				&kms.DescribeKeyInput{KeyId: key.KeyId, GrantTokens: []string{*parent.GrantToken}},
			)
			mustAuthorize(t, err) // constraint exception

			for _, child := range []struct {
				constraint *kmstypes.GrantConstraints
				operations []kmstypes.GrantOperation
				allowed    bool
			}{
				{constraint, []kmstypes.GrantOperation{"Encrypt"}, true},
				{
					&kmstypes.GrantConstraints{EncryptionContextEquals: map[string]string{"app": "web"}},
					[]kmstypes.GrantOperation{"Encrypt"},
					true,
				},
				{
					&kmstypes.GrantConstraints{EncryptionContextSubset: map[string]string{"app": "web", "extra": "value"}},
					[]kmstypes.GrantOperation{"Encrypt"},
					!equals,
				},
				{nil, []kmstypes.GrantOperation{"Encrypt"}, false},
				{
					&kmstypes.GrantConstraints{EncryptionContextEquals: map[string]string{"app": "other"}},
					[]kmstypes.GrantOperation{"Encrypt"},
					false,
				},
				{constraint, []kmstypes.GrantOperation{"GenerateDataKeyWithoutPlaintext"}, false},
			} {
				out, callErr := user.CreateGrant(ctx, &kms.CreateGrantInput{
					KeyId:            key.KeyId,
					GranteePrincipal: &grantee,
					Operations:       child.operations,
					Constraints:      child.constraint,
					GrantTokens:      []string{*parent.GrantToken},
				})
				if child.allowed {
					mustAuthorize(t, callErr)
					_, err = root.RevokeGrant(ctx, &kms.RevokeGrantInput{KeyId: key.KeyId, GrantId: out.GrantId})
					mustAuthorize(t, err)
				} else {
					wantCode(t, callErr, "AccessDeniedException")
				}
			}

			_, err = user.RetireGrant(ctx, &kms.RetireGrantInput{KeyId: key.KeyId, GrantId: parent.GrantId})
			mustAuthorize(t, err) // constraint exception
		})
	}
}

func TestKMSGrantSoftModeAndTokens(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationSoft, false)
	ctx := context.Background()
	issued := authorizationUser(t, s)
	root := kmsClient(s, "test", "us-east-1")
	user := kmsClient(s, *issued.AccessKeyId, "us-east-1")
	key := createKey(t, root, "soft-grants")
	_, err := root.CreateGrant(ctx, &kms.CreateGrantInput{
		KeyId: key.KeyId, GranteePrincipal: new("arn:aws:iam::" + testConfig().Defaults.Account + ":user/apps/alice"),
		Operations: []kmstypes.GrantOperation{"DescribeKey"},
	})
	mustAuthorize(t, err)
	_, err = user.Encrypt(ctx, &kms.EncryptInput{KeyId: key.KeyId, Plaintext: []byte("soft")})
	mustAuthorize(t, err)
	response, err := http.Get(s.URL + "/_tinystack/requests")
	mustAuthorize(t, err)
	defer response.Body.Close()
	var records struct {
		Requests []gateway.Record `json:"requests"`
	}
	mustAuthorize(t, json.NewDecoder(response.Body).Decode(&records))
	found := false
	for _, record := range records.Requests {
		if record.Operation == "Encrypt" && record.Authorization != nil {
			found = !record.Authorization.Allowed && len(record.Authorization.Checks) == 1 &&
				record.Authorization.Checks[0].Grant != nil && record.Authorization.Checks[0].Grant.Reason != ""
		}
	}

	if !found {
		t.Fatal("missing would-deny grant decision")
	}

	// Token shape validation also applies with authorization disabled.
	off := kmsClient(server(t), "test", "us-east-1")
	offKey := createKey(t, off, "tokens")
	for _, tokens := range [][]string{{""}, make([]string, 11)} {
		_, err = off.Encrypt(ctx, &kms.EncryptInput{KeyId: offKey.KeyId, Plaintext: []byte("x"), GrantTokens: tokens})
		wantCode(t, err, "ValidationException")
	}
}

func TestKMSGrantRoleAndAccountPrincipals(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, false)
	ctx := context.Background()
	root := kmsClient(s, "test", "us-east-1")
	key := createKey(t, root, "role-grants")
	role, err := iamClient(s, "test", "us-east-1").CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: new("grant-user"), Path: new("/apps/"),
		AssumeRolePolicyDocument: new(`{"Statement":{"Effect":"Allow","Principal":{"AWS":"*"},
"Action":"sts:AssumeRole"}}`),
	})
	mustAuthorize(t, err)
	assumed, err := stsClient(s, "test", "test").AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: role.Role.Arn, RoleSessionName: new("grantee"),
	})
	mustAuthorize(t, err)
	session := kms.New(kms.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL),
		Credentials: temporary(assumed.Credentials), Retryer: aws.NopRetryer{},
	})
	for _, principal := range []string{*role.Role.Arn, "arn:aws:iam::" + testConfig().Defaults.Account + ":root"} {
		grant, createErr := root.CreateGrant(ctx, &kms.CreateGrantInput{
			KeyId: key.KeyId, GranteePrincipal: &principal,
			Operations: []kmstypes.GrantOperation{"Encrypt"},
		})
		mustAuthorize(t, createErr)
		_, err = session.Encrypt(ctx, &kms.EncryptInput{KeyId: key.KeyId, Plaintext: []byte("role")})
		mustAuthorize(t, err)
		// Owning root may retire even without being named in the grant.
		_, err = root.RetireGrant(ctx, &kms.RetireGrantInput{KeyId: key.KeyId, GrantId: grant.GrantId})
		mustAuthorize(t, err)
		_, err = session.Encrypt(ctx, &kms.EncryptInput{KeyId: key.KeyId, Plaintext: []byte("role")})
		wantCode(t, err, "AccessDeniedException")
	}
}

func TestKMSGrantRetirementDelegation(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, false)
	ctx := context.Background()
	issued := authorizationUser(t, s)
	root := kmsClient(s, "test", "us-east-1")
	user := kmsClient(s, *issued.AccessKeyId, "us-east-1")
	key := createKey(t, root, "retirement")
	grant, err := root.CreateGrant(ctx, &kms.CreateGrantInput{
		KeyId: key.KeyId, GranteePrincipal: new("arn:aws:iam::333333333333:role/other"),
		Operations: []kmstypes.GrantOperation{"Encrypt"},
	})
	mustAuthorize(t, err)
	// A direct key-policy Allow does not suffice.
	document := fmt.Sprintf(`{"Statement":[{"Effect":"Allow","Principal":{"AWS":%q},
"Action":"kms:*","Resource":"*"},{"Effect":"Allow","Principal":{"AWS":%q},
"Action":"kms:RetireGrant","Resource":"*"}]}`,
		"arn:aws:iam::"+testConfig().Defaults.Account+":root",
		"arn:aws:iam::"+testConfig().Defaults.Account+":user/apps/alice")
	_, err = root.PutKeyPolicy(ctx, &kms.PutKeyPolicyInput{KeyId: key.KeyId, Policy: &document})
	mustAuthorize(t, err)
	_, err = user.RetireGrant(ctx, &kms.RetireGrantInput{KeyId: key.KeyId, GrantId: grant.GrantId})
	wantCode(t, err, "AccessDeniedException")
	authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow","Action":"kms:RetireGrant","Resource":"*"}}`)
	_, err = user.RetireGrant(ctx, &kms.RetireGrantInput{KeyId: key.KeyId, GrantId: grant.GrantId})
	mustAuthorize(t, err)
}

func TestKMSGrantUnsupportedPrincipal(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, false)
	ctx := context.Background()
	issued := authorizationUser(t, s)
	root := kmsClient(s, "test", "us-east-1")
	key := createKey(t, root, "unsupported")
	_, err := root.CreateGrant(ctx, &kms.CreateGrantInput{
		KeyId: key.KeyId, GranteePrincipal: new("*"), Operations: []kmstypes.GrantOperation{"Encrypt"},
	})
	mustAuthorize(t, err)
	_, err = kmsClient(s, *issued.AccessKeyId, "us-east-1").Encrypt(ctx, &kms.EncryptInput{
		KeyId: key.KeyId, Plaintext: []byte("unsupported"),
	})
	wantCode(t, err, "AccessDeniedException")
	if !strings.Contains(err.Error(), "unsupported grant principal") {
		t.Fatalf("missing unsupported construct explanation: %v", err)
	}
}
