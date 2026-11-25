package integration_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"golang.org/x/crypto/ssh"

	"github.com/amjadjibon/tinystack/internal/config"
)

func testTOTPCode(t *testing.T, base32Seed []byte, step int64) string {
	t.Helper()
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(
		strings.TrimRight(string(base32Seed), "="),
	)
	if err != nil {
		t.Fatal(err)
	}

	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	code := (int64(sum[offset]&0x7f) << 24) |
		(int64(sum[offset+1]) << 16) |
		(int64(sum[offset+2]) << 8) |
		int64(sum[offset+3])
	return fmt.Sprintf("%06d", code%1_000_000)
}

func testSelfSignedCert(t *testing.T, expiry time.Time) (certPEM, keyPEM string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test.tinystack.local"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     expiry,
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}

func testSSHPublicKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}

	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
}

func TestIAMAccountAliasAndPasswordPolicy(t *testing.T) {
	s := server(t)
	c := iamClient(s, "test", "us-east-1")
	other := iamClient(s, "team-b", "us-east-1")
	ctx := context.Background()

	// Account alias validation and lifecycle.
	for _, bad := range []string{"ab", "-abc", "abc-", "a--b", "BadCase", strings.Repeat("a", 64)} {
		_, err := c.CreateAccountAlias(ctx, &iam.CreateAccountAliasInput{AccountAlias: new(bad)})
		wantCode(t, err, "ValidationError")
	}

	if _, err := c.CreateAccountAlias(ctx, &iam.CreateAccountAliasInput{
		AccountAlias: new("my-org-1"),
	}); err != nil {
		t.Fatal(err)
	}

	aliases, err := c.ListAccountAliases(ctx, &iam.ListAccountAliasesInput{})
	if err != nil || !slices.Equal(aliases.AccountAliases, []string{"my-org-1"}) {
		t.Fatalf("aliases: %+v %v", aliases, err)
	}

	// Cross-account conflict.
	_, err = other.CreateAccountAlias(ctx, &iam.CreateAccountAliasInput{AccountAlias: new("my-org-1")})
	wantCode(t, err, "EntityAlreadyExists")

	// Replacing alias on the same account frees the old one.
	if _, err = c.CreateAccountAlias(ctx, &iam.CreateAccountAliasInput{
		AccountAlias: new("my-org-2"),
	}); err != nil {
		t.Fatal(err)
	}

	aliases, err = c.ListAccountAliases(ctx, &iam.ListAccountAliasesInput{})
	if err != nil || !slices.Equal(aliases.AccountAliases, []string{"my-org-2"}) {
		t.Fatalf("replaced aliases: %+v %v", aliases, err)
	}

	if _, err = other.CreateAccountAlias(ctx, &iam.CreateAccountAliasInput{
		AccountAlias: new("my-org-1"),
	}); err != nil {
		t.Fatalf("expected freed alias to succeed: %v", err)
	}

	_, err = c.DeleteAccountAlias(ctx, &iam.DeleteAccountAliasInput{AccountAlias: new("my-org-1")})
	wantCode(t, err, "NoSuchEntity")

	if _, err = c.DeleteAccountAlias(ctx, &iam.DeleteAccountAliasInput{
		AccountAlias: new("my-org-2"),
	}); err != nil {
		t.Fatal(err)
	}

	aliases, err = c.ListAccountAliases(ctx, &iam.ListAccountAliasesInput{})
	if err != nil || len(aliases.AccountAliases) != 0 {
		t.Fatalf("after delete: %+v %v", aliases, err)
	}

	// Account password policy.
	_, err = c.GetAccountPasswordPolicy(ctx, &iam.GetAccountPasswordPolicyInput{})
	wantCode(t, err, "NoSuchEntity")
	_, err = c.DeleteAccountPasswordPolicy(ctx, &iam.DeleteAccountPasswordPolicyInput{})
	wantCode(t, err, "NoSuchEntity")

	_, err = c.UpdateAccountPasswordPolicy(ctx, &iam.UpdateAccountPasswordPolicyInput{
		MinimumPasswordLength: aws.Int32(5),
	})
	wantCode(t, err, "ValidationError")
	_, err = c.UpdateAccountPasswordPolicy(ctx, &iam.UpdateAccountPasswordPolicyInput{
		MaxPasswordAge: aws.Int32(2000),
	})
	wantCode(t, err, "ValidationError")
	_, err = c.UpdateAccountPasswordPolicy(ctx, &iam.UpdateAccountPasswordPolicyInput{
		PasswordReusePrevention: aws.Int32(25),
	})
	wantCode(t, err, "ValidationError")

	if _, err = c.UpdateAccountPasswordPolicy(ctx, &iam.UpdateAccountPasswordPolicyInput{
		MinimumPasswordLength:      aws.Int32(12),
		RequireSymbols:             true,
		RequireNumbers:             true,
		RequireUppercaseCharacters: true,
		RequireLowercaseCharacters: true,
		AllowUsersToChangePassword: true,
		MaxPasswordAge:             aws.Int32(90),
		PasswordReusePrevention:    aws.Int32(5),
		HardExpiry:                 aws.Bool(true),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := c.GetAccountPasswordPolicy(ctx, &iam.GetAccountPasswordPolicyInput{})
	if err != nil {
		t.Fatal(err)
	}

	p := got.PasswordPolicy
	if aws.ToInt32(p.MinimumPasswordLength) != 12 || !p.RequireSymbols || !p.RequireNumbers ||
		!p.RequireUppercaseCharacters || !p.RequireLowercaseCharacters || !p.AllowUsersToChangePassword ||
		!p.ExpirePasswords || aws.ToInt32(p.MaxPasswordAge) != 90 ||
		aws.ToInt32(p.PasswordReusePrevention) != 5 || !aws.ToBool(p.HardExpiry) {
		t.Fatalf("unexpected password policy: %+v", p)
	}

	if _, err = c.DeleteAccountPasswordPolicy(ctx, &iam.DeleteAccountPasswordPolicyInput{}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetAccountPasswordPolicy(ctx, &iam.GetAccountPasswordPolicyInput{})
	wantCode(t, err, "NoSuchEntity")
}

func TestIAMAccountSummaryAndAuthorizationDetails(t *testing.T) {
	s := server(t)
	c := iamClient(s, "test", "us-east-1")
	ctx := context.Background()

	if _, err := c.CreateUser(ctx, &iam.CreateUserInput{
		UserName: new("alice"),
		Path:     new("/eng/"),
		Tags:     []iamtypes.Tag{{Key: new("env"), Value: new("prod")}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.CreateGroup(ctx, &iam.CreateGroupInput{GroupName: new("devs")}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.AddUserToGroup(ctx, &iam.AddUserToGroupInput{
		GroupName: new("devs"), UserName: new("alice"),
	}); err != nil {
		t.Fatal(err)
	}

	roleOut, err := c.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: new("app-role"),
		AssumeRolePolicyDocument: new(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Principal":{"AWS":"arn:aws:iam::000000000000:root"},"Action":"sts:AssumeRole"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{
		InstanceProfileName: new("app-profile"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: new("app-profile"), RoleName: new("app-role"),
	}); err != nil {
		t.Fatal(err)
	}

	localPol, err := c.CreatePolicy(ctx, &iam.CreatePolicyInput{
		PolicyName: new("local-read"), PolicyDocument: new(s3ReadPolicy),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreatePolicyVersion(ctx, &iam.CreatePolicyVersionInput{
		PolicyArn: localPol.Policy.Arn, PolicyDocument: new(s3ReadPolicy),
	}); err != nil {
		t.Fatal(err)
	}

	awsLambdaBasic := "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
	if _, err = c.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
		RoleName: new("app-role"), PolicyArn: new(awsLambdaBasic),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.AttachUserPolicy(ctx, &iam.AttachUserPolicyInput{
		UserName: new("alice"), PolicyArn: localPol.Policy.Arn,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.PutUserPolicy(ctx, &iam.PutUserPolicyInput{
		UserName: new("alice"), PolicyName: new("inline-s3"), PolicyDocument: new(s3ReadPolicy),
	}); err != nil {
		t.Fatal(err)
	}

	// Assume role so RoleLastUsed is populated.
	stsEU := sts.New(sts.Options{
		Region:       "eu-west-1",
		BaseEndpoint: new(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
	if _, err = stsEU.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: roleOut.Role.Arn, RoleSessionName: new("audit"),
	}); err != nil {
		t.Fatal(err)
	}

	certPEM, keyPEM := testSelfSignedCert(t, time.Now().Add(24*time.Hour))
	if _, err = c.UploadServerCertificate(ctx, &iam.UploadServerCertificateInput{
		ServerCertificateName: new("web-cert"), CertificateBody: new(certPEM), PrivateKey: new(keyPEM),
	}); err != nil {
		t.Fatal(err)
	}

	mfaOut, err := c.CreateVirtualMFADevice(ctx, &iam.CreateVirtualMFADeviceInput{
		VirtualMFADeviceName: new("alice-mfa"),
	})
	if err != nil {
		t.Fatal(err)
	}

	step := time.Now().Unix() / 30
	if _, err = c.EnableMFADevice(ctx, &iam.EnableMFADeviceInput{
		UserName:            new("alice"),
		SerialNumber:        mfaOut.VirtualMFADevice.SerialNumber,
		AuthenticationCode1: new(testTOTPCode(t, mfaOut.VirtualMFADevice.Base32StringSeed, step)),
		AuthenticationCode2: new(testTOTPCode(t, mfaOut.VirtualMFADevice.Base32StringSeed, step+1)),
	}); err != nil {
		t.Fatal(err)
	}

	// GetAccountSummary
	sum, err := c.GetAccountSummary(ctx, &iam.GetAccountSummaryInput{})
	if err != nil {
		t.Fatal(err)
	}

	for k, want := range map[string]int32{
		"Users":                        1,
		"Groups":                       1,
		"Roles":                        1,
		"Policies":                     1,
		"PolicyVersionsInUse":          2,
		"InstanceProfiles":             1,
		"ServerCertificates":           1,
		"MFADevices":                   1,
		"MFADevicesInUse":              1,
		"UsersQuota":                   5000,
		"RolesQuota":                   1000,
		"AccessKeysPerUserQuota":       2,
		"AttachedPoliciesPerRoleQuota": 10,
	} {
		if got := sum.SummaryMap[k]; got != want {
			t.Fatalf("summary[%s] = %d, want %d", k, got, want)
		}
	}

	// GetAccountAuthorizationDetails: filtered and paginated.
	all, err := c.GetAccountAuthorizationDetails(ctx, &iam.GetAccountAuthorizationDetailsInput{})
	if err != nil {
		t.Fatal(err)
	}

	if len(all.UserDetailList) != 1 || len(all.GroupDetailList) != 1 ||
		len(all.RoleDetailList) != 1 || len(all.Policies) != 2 {
		t.Fatalf("unexpected details counts: users=%d groups=%d roles=%d policies=%d",
			len(all.UserDetailList), len(all.GroupDetailList), len(all.RoleDetailList), len(all.Policies))
	}

	u := all.UserDetailList[0]
	if !slices.Equal(u.GroupList, []string{"devs"}) || len(u.UserPolicyList) != 1 ||
		len(u.AttachedManagedPolicies) != 1 || len(u.Tags) != 1 {
		t.Fatalf("unexpected user detail: %+v", u)
	}

	r := all.RoleDetailList[0]
	if len(r.InstanceProfileList) != 1 || r.RoleLastUsed == nil ||
		r.RoleLastUsed.LastUsedDate == nil || aws.ToString(r.RoleLastUsed.Region) != "eu-west-1" {
		t.Fatalf("unexpected role detail: %+v", r)
	}

	// Pagination across all 5 entities with MaxItems=2.
	var pagedCount int
	var marker *string
	for {
		pageOut, pageErr := c.GetAccountAuthorizationDetails(ctx, &iam.GetAccountAuthorizationDetailsInput{
			MaxItems: aws.Int32(2), Marker: marker,
		})
		if pageErr != nil {
			t.Fatal(pageErr)
		}

		pagedCount += len(pageOut.UserDetailList) + len(pageOut.GroupDetailList) +
			len(pageOut.RoleDetailList) + len(pageOut.Policies)
		if !pageOut.IsTruncated {
			break
		}

		marker = pageOut.Marker
	}

	if pagedCount != 5 {
		t.Fatalf("paginated total = %d, want 5", pagedCount)
	}

	// Filter to AWSManagedPolicy only.
	awsOnly, err := c.GetAccountAuthorizationDetails(ctx, &iam.GetAccountAuthorizationDetailsInput{
		Filter: []iamtypes.EntityType{iamtypes.EntityTypeAWSManagedPolicy},
	})
	if err != nil || len(awsOnly.Policies) != 1 ||
		aws.ToString(awsOnly.Policies[0].Arn) != awsLambdaBasic ||
		len(awsOnly.Policies[0].PolicyVersionList) != 1 {
		t.Fatalf("aws filter: %+v %v", awsOnly, err)
	}
}

func TestIAMPolicyCatalogEntitiesAndContextKeys(t *testing.T) {
	s := server(t)
	c := iamClient(s, "test", "us-east-1")
	ctx := context.Background()

	awsBasicARN := "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
	awsS3RO := "arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess"

	// Read cataloged AWS-managed policy, its version, and version list.
	gotPol, err := c.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: new(awsBasicARN)})
	if err != nil || aws.ToString(gotPol.Policy.PolicyName) != "AWSLambdaBasicExecutionRole" ||
		aws.ToString(gotPol.Policy.Path) != "/service-role/" || aws.ToString(gotPol.Policy.DefaultVersionId) != "v1" {
		t.Fatalf("get aws policy: %+v %v", gotPol, err)
	}

	ver, err := c.GetPolicyVersion(ctx, &iam.GetPolicyVersionInput{
		PolicyArn: new(awsBasicARN), VersionId: new("v1"),
	})
	if err != nil || !strings.Contains(decoded(t, ver.PolicyVersion.Document), "logs:PutLogEvents") {
		t.Fatalf("get aws policy version: %+v %v", ver, err)
	}

	vers, err := c.ListPolicyVersions(ctx, &iam.ListPolicyVersionsInput{PolicyArn: new(awsBasicARN)})
	if err != nil || len(vers.Versions) != 1 || !vers.Versions[0].IsDefaultVersion {
		t.Fatalf("list aws policy versions: %+v %v", vers, err)
	}

	// Unknown AWS-managed policy still returns NoSuchEntity on GetPolicy.
	_, err = c.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: new("arn:aws:iam::aws:policy/DoesNotExist")})
	wantCode(t, err, "NoSuchEntity")

	// ListPolicies Scope: AWS with PathPrefix /service-role/.
	awsServiceRolePolicies, err := c.ListPolicies(ctx, &iam.ListPoliciesInput{
		Scope: iamtypes.PolicyScopeTypeAws, PathPrefix: new("/service-role/"),
	})
	if err != nil || len(awsServiceRolePolicies.Policies) < 5 {
		t.Fatalf("list service-role aws policies: %+v %v", awsServiceRolePolicies, err)
	}

	// Attach policies & boundaries and test PolicyUsageFilter + ListEntitiesForPolicy.
	if _, err = c.CreateUser(ctx, &iam.CreateUserInput{
		UserName: new("u1"), Path: new("/team/"), PermissionsBoundary: new(awsS3RO),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreateGroup(ctx, &iam.CreateGroupInput{GroupName: new("g1")}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: new("r1"), AssumeRolePolicyDocument: new(trustPolicy),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.AttachGroupPolicy(ctx, &iam.AttachGroupPolicyInput{
		GroupName: new("g1"), PolicyArn: new(awsS3RO),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
		RoleName: new("r1"), PolicyArn: new(awsBasicARN),
	}); err != nil {
		t.Fatal(err)
	}

	// Live counts on GetPolicy for AWS-managed policy.
	s3Pol, err := c.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: new(awsS3RO)})
	if err != nil || aws.ToInt32(s3Pol.Policy.AttachmentCount) != 1 ||
		aws.ToInt32(s3Pol.Policy.PermissionsBoundaryUsageCount) != 1 {
		t.Fatalf("live counts: %+v %v", s3Pol, err)
	}

	// ListPolicies with PolicyUsageFilter=PermissionsBoundary.
	boundaryOnly, err := c.ListPolicies(ctx, &iam.ListPoliciesInput{
		Scope: iamtypes.PolicyScopeTypeAll, PolicyUsageFilter: iamtypes.PolicyUsageTypePermissionsBoundary,
	})
	if err != nil || len(boundaryOnly.Policies) != 1 || aws.ToString(boundaryOnly.Policies[0].Arn) != awsS3RO {
		t.Fatalf("boundary filter: %+v %v", boundaryOnly, err)
	}

	// ListEntitiesForPolicy on AWS-managed policy.
	entities, err := c.ListEntitiesForPolicy(ctx, &iam.ListEntitiesForPolicyInput{PolicyArn: new(awsS3RO)})
	if err != nil || len(entities.PolicyGroups) != 1 || len(entities.PolicyUsers) != 1 ||
		len(entities.PolicyRoles) != 0 {
		t.Fatalf("entities: %+v %v", entities, err)
	}

	boundaryEntities, err := c.ListEntitiesForPolicy(ctx, &iam.ListEntitiesForPolicyInput{
		PolicyArn: new(awsS3RO), PolicyUsageFilter: iamtypes.PolicyUsageTypePermissionsBoundary,
	})
	if err != nil || len(boundaryEntities.PolicyUsers) != 1 || len(boundaryEntities.PolicyGroups) != 0 {
		t.Fatalf("boundary entities: %+v %v", boundaryEntities, err)
	}

	// SimulatePrincipalPolicy with service-role AWS-managed policy.
	sim, err := c.SimulatePrincipalPolicy(ctx, &iam.SimulatePrincipalPolicyInput{
		PolicySourceArn: new("arn:aws:iam::000000000000:role/r1"),
		ActionNames:     []string{"logs:PutLogEvents", "s3:PutObject"},
	})
	if err != nil || string(sim.EvaluationResults[0].EvalDecision) != "allowed" ||
		string(sim.EvaluationResults[1].EvalDecision) != "implicitDeny" {
		t.Fatalf("simulate service-role policy: %+v %v", sim, err)
	}

	// GetContextKeysForCustomPolicy and GetContextKeysForPrincipalPolicy.
	condPolicy := `{"Version":"2012-10-17","Statement":[` +
		`{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/${aws:username}/*",` +
		`"Condition":{"StringEquals":{"aws:RequestedRegion":"us-east-1","s3:prefix":"home/${aws:username}"},` +
		`"Bool":{"aws:SecureTransport":"true"}}}]}`
	customKeys, err := c.GetContextKeysForCustomPolicy(ctx, &iam.GetContextKeysForCustomPolicyInput{
		PolicyInputList: []string{condPolicy},
	})
	if err != nil {
		t.Fatal(err)
	}

	wantKeys := []string{"aws:RequestedRegion", "aws:SecureTransport", "aws:username", "s3:prefix"}
	if !slices.Equal(customKeys.ContextKeyNames, wantKeys) {
		t.Fatalf("custom context keys: got %v, want %v", customKeys.ContextKeyNames, wantKeys)
	}

	if _, err = c.PutUserPolicy(ctx, &iam.PutUserPolicyInput{
		UserName: new("u1"), PolicyName: new("cond"), PolicyDocument: new(condPolicy),
	}); err != nil {
		t.Fatal(err)
	}

	principalKeys, err := c.GetContextKeysForPrincipalPolicy(ctx, &iam.GetContextKeysForPrincipalPolicyInput{
		PolicySourceArn: new("arn:aws:iam::000000000000:user/team/u1"),
	})
	if err != nil || !slices.Equal(principalKeys.ContextKeyNames, wantKeys) {
		t.Fatalf("principal context keys: %+v %v", principalKeys, err)
	}
}

func TestIAMAccessKeyLastUsedAndRoleLastUsed(t *testing.T) {
	for _, verify := range []bool{false, true} {
		t.Run(fmt.Sprintf("verify=%t", verify), func(t *testing.T) {
			cfg := testConfig()
			if verify {
				cfg.Identity.Mode = config.IdentityVerify
				cfg.Identity.SecretKeys = map[string]string{"test": "test"}
			}

			s := serverConfig(t, cfg, &fakeRuntime{})
			ctx := context.Background()
			root := iamClient(s, "test", "us-east-1")

			if _, err := root.CreateUser(ctx, &iam.CreateUserInput{UserName: new("alice")}); err != nil {
				t.Fatal(err)
			}

			keyOut, err := root.CreateAccessKey(ctx, &iam.CreateAccessKeyInput{UserName: new("alice")})
			if err != nil {
				t.Fatal(err)
			}

			keyID := aws.ToString(keyOut.AccessKey.AccessKeyId)
			secret := aws.ToString(keyOut.AccessKey.SecretAccessKey)

			// Before use: N/A and nil LastUsedDate.
			before, err := root.GetAccessKeyLastUsed(ctx, &iam.GetAccessKeyLastUsedInput{AccessKeyId: new(keyID)})
			if err != nil || before.AccessKeyLastUsed.LastUsedDate != nil ||
				aws.ToString(before.AccessKeyLastUsed.ServiceName) != "N/A" ||
				aws.ToString(before.AccessKeyLastUsed.Region) != "N/A" {
				t.Fatalf("before use: %+v %v", before, err)
			}

			if verify {
				// Bad signature must not record usage.
				badS3 := s3Client(s, keyID, "eu-west-1", func(o *s3.Options) {
					o.Credentials = aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
						return aws.Credentials{AccessKeyID: keyID, SecretAccessKey: "wrong-secret"}, nil
					})
				})
				_, err = badS3.ListBuckets(ctx, &s3.ListBucketsInput{})
				wantCode(t, err, "SignatureDoesNotMatch")

				stillUnused, checkErr := root.GetAccessKeyLastUsed(ctx, &iam.GetAccessKeyLastUsedInput{
					AccessKeyId: new(keyID),
				})
				if checkErr != nil || stillUnused.AccessKeyLastUsed.LastUsedDate != nil {
					t.Fatalf("bad signature recorded usage: %+v %v", stillUnused, checkErr)
				}
			}

			// Authenticated S3 call in eu-west-1 records usage.
			userS3 := s3Client(s, keyID, "eu-west-1", func(o *s3.Options) {
				o.Credentials = aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
					return aws.Credentials{AccessKeyID: keyID, SecretAccessKey: secret}, nil
				})
			})
			if _, err = userS3.ListBuckets(ctx, &s3.ListBucketsInput{}); err != nil {
				t.Fatal(err)
			}

			afterS3, err := root.GetAccessKeyLastUsed(ctx, &iam.GetAccessKeyLastUsedInput{AccessKeyId: new(keyID)})
			if err != nil || afterS3.AccessKeyLastUsed.LastUsedDate == nil ||
				aws.ToString(afterS3.AccessKeyLastUsed.ServiceName) != "s3" ||
				aws.ToString(afterS3.AccessKeyLastUsed.Region) != "eu-west-1" {
				t.Fatalf("after S3: %+v %v", afterS3, err)
			}

			// Authenticated DynamoDB call in us-west-2 updates usage.
			userDDB := dynamodb.New(dynamodb.Options{
				Region:       "us-west-2",
				BaseEndpoint: new(s.URL),
				Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
					return aws.Credentials{AccessKeyID: keyID, SecretAccessKey: secret}, nil
				}),
				Retryer: aws.NopRetryer{},
			})
			if _, err = userDDB.ListTables(ctx, &dynamodb.ListTablesInput{}); err != nil {
				t.Fatal(err)
			}

			afterDDB, err := root.GetAccessKeyLastUsed(ctx, &iam.GetAccessKeyLastUsedInput{AccessKeyId: new(keyID)})
			if err != nil || afterDDB.AccessKeyLastUsed.LastUsedDate == nil ||
				aws.ToString(afterDDB.AccessKeyLastUsed.ServiceName) != "dynamodb" ||
				aws.ToString(afterDDB.AccessKeyLastUsed.Region) != "us-west-2" {
				t.Fatalf("after DDB: %+v %v", afterDDB, err)
			}

			// RoleLastUsed: initially nil, populated after STS AssumeRole.
			roleOut, err := root.CreateRole(ctx, &iam.CreateRoleInput{
				RoleName: new("tracked-role"),
				AssumeRolePolicyDocument: new(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
					`"Principal":{"AWS":"arn:aws:iam::000000000000:root"},"Action":"sts:AssumeRole"}]}`),
			})
			if err != nil {
				t.Fatal(err)
			}

			if roleOut.Role.RoleLastUsed != nil && roleOut.Role.RoleLastUsed.LastUsedDate != nil {
				t.Fatalf("expected unused role: %+v", roleOut.Role.RoleLastUsed)
			}

			stsAP := sts.New(sts.Options{
				Region:       "ap-southeast-1",
				BaseEndpoint: new(s.URL),
				Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
					return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
				}),
				Retryer: aws.NopRetryer{},
			})
			if _, err = stsAP.AssumeRole(ctx, &sts.AssumeRoleInput{
				RoleArn: roleOut.Role.Arn, RoleSessionName: new("track"),
			}); err != nil {
				t.Fatal(err)
			}

			gotRole, err := root.GetRole(ctx, &iam.GetRoleInput{RoleName: new("tracked-role")})
			if err != nil || gotRole.Role.RoleLastUsed == nil || gotRole.Role.RoleLastUsed.LastUsedDate == nil ||
				aws.ToString(gotRole.Role.RoleLastUsed.Region) != "ap-southeast-1" {
				t.Fatalf("GetRole RoleLastUsed: %+v %v", gotRole, err)
			}

			listedRoles, err := root.ListRoles(ctx, &iam.ListRolesInput{})
			if err != nil || len(listedRoles.Roles) != 1 || listedRoles.Roles[0].RoleLastUsed == nil ||
				aws.ToString(listedRoles.Roles[0].RoleLastUsed.Region) != "ap-southeast-1" {
				t.Fatalf("ListRoles RoleLastUsed: %+v %v", listedRoles, err)
			}
		})
	}
}

func TestIAMLoginProfilesAndChangePassword(t *testing.T) {
	s := server(t)
	root := iamClient(s, "test", "us-east-1")
	ctx := context.Background()

	if _, err := root.CreateUser(ctx, &iam.CreateUserInput{UserName: new("alice")}); err != nil {
		t.Fatal(err)
	}

	if _, err := root.UpdateAccountPasswordPolicy(ctx, &iam.UpdateAccountPasswordPolicyInput{
		MinimumPasswordLength:      aws.Int32(10),
		RequireUppercaseCharacters: true,
		RequireLowercaseCharacters: true,
		RequireNumbers:             true,
		RequireSymbols:             true,
		PasswordReusePrevention:    aws.Int32(2),
		AllowUsersToChangePassword: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Weak password fails policy.
	_, err := root.CreateLoginProfile(ctx, &iam.CreateLoginProfileInput{
		UserName: new("alice"), Password: new("weak"),
	})
	wantCode(t, err, "PasswordPolicyViolation")

	created, err := root.CreateLoginProfile(ctx, &iam.CreateLoginProfileInput{
		UserName: new("alice"), Password: new("StrongPass1!"), PasswordResetRequired: true,
	})
	if err != nil || aws.ToString(created.LoginProfile.UserName) != "alice" ||
		!created.LoginProfile.PasswordResetRequired {
		t.Fatalf("create login profile: %+v %v", created, err)
	}

	// Duplicate fails.
	_, err = root.CreateLoginProfile(ctx, &iam.CreateLoginProfileInput{
		UserName: new("alice"), Password: new("StrongPass2!"),
	})
	wantCode(t, err, "EntityAlreadyExists")

	// UpdateLoginProfile rejects reused password, accepts new strong password.
	_, err = root.UpdateLoginProfile(ctx, &iam.UpdateLoginProfileInput{
		UserName: new("alice"), Password: new("StrongPass1!"),
	})
	wantCode(t, err, "PasswordPolicyViolation")

	if _, err = root.UpdateLoginProfile(ctx, &iam.UpdateLoginProfileInput{
		UserName: new("alice"), Password: new("StrongPass2!"), PasswordResetRequired: aws.Bool(true),
	}); err != nil {
		t.Fatal(err)
	}

	// ChangePassword as root fails with InvalidUserType.
	_, err = root.ChangePassword(ctx, &iam.ChangePasswordInput{
		OldPassword: new("StrongPass2!"), NewPassword: new("StrongPass3!"),
	})
	wantCode(t, err, "InvalidUserType")

	// ChangePassword as IAM user.
	keyOut, err := root.CreateAccessKey(ctx, &iam.CreateAccessKeyInput{UserName: new("alice")})
	if err != nil {
		t.Fatal(err)
	}

	aliceIAM := iamSigned(s, aws.ToString(keyOut.AccessKey.AccessKeyId), aws.ToString(keyOut.AccessKey.SecretAccessKey))
	_, err = aliceIAM.ChangePassword(ctx, &iam.ChangePasswordInput{
		OldPassword: new("WrongPass1!"), NewPassword: new("StrongPass3!"),
	})
	wantCode(t, err, "AccessDenied")

	_, err = aliceIAM.ChangePassword(ctx, &iam.ChangePasswordInput{
		OldPassword: new("StrongPass2!"), NewPassword: new("StrongPass1!"),
	})
	wantCode(t, err, "PasswordPolicyViolation")

	if _, err = aliceIAM.ChangePassword(ctx, &iam.ChangePasswordInput{
		OldPassword: new("StrongPass2!"), NewPassword: new("StrongPass3!"),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := root.GetLoginProfile(ctx, &iam.GetLoginProfileInput{UserName: new("alice")})
	if err != nil || got.LoginProfile.PasswordResetRequired {
		t.Fatalf("after ChangePassword: %+v %v", got, err)
	}

	// DeleteUser blocked until login profile and access key are removed.
	if _, err = root.DeleteAccessKey(ctx, &iam.DeleteAccessKeyInput{
		UserName: new("alice"), AccessKeyId: keyOut.AccessKey.AccessKeyId,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = root.DeleteUser(ctx, &iam.DeleteUserInput{UserName: new("alice")})
	wantCode(t, err, "DeleteConflict")

	if _, err = root.DeleteLoginProfile(ctx, &iam.DeleteLoginProfileInput{UserName: new("alice")}); err != nil {
		t.Fatal(err)
	}

	if _, err = root.DeleteUser(ctx, &iam.DeleteUserInput{UserName: new("alice")}); err != nil {
		t.Fatal(err)
	}
}

func TestIAMSAMLProvidersOIDCTagsAndMFA(t *testing.T) {
	s := server(t)
	c := iamClient(s, "test", "us-east-1")
	tagAPI := resourcegroupstaggingapi.New(resourcegroupstaggingapi.Options{
		Region:       "us-east-1",
		BaseEndpoint: new(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
	ctx := context.Background()

	// SAML providers.
	_, err := c.CreateSAMLProvider(ctx, &iam.CreateSAMLProviderInput{
		Name: new("corp"), SAMLMetadataDocument: new("not xml"),
	})
	wantCode(t, err, "InvalidInput")

	samlXML := `<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" validUntil="2035-01-01T00:00:00Z">` +
		`<IDPSSODescriptor/></EntityDescriptor>`
	createdSAML, err := c.CreateSAMLProvider(ctx, &iam.CreateSAMLProviderInput{
		Name: new("corp"), SAMLMetadataDocument: new(samlXML),
		Tags: []iamtypes.Tag{{Key: new("team"), Value: new("sec")}},
	})
	if err != nil || aws.ToString(createdSAML.SAMLProviderArn) != "arn:aws:iam::000000000000:saml-provider/corp" {
		t.Fatalf("create saml: %+v %v", createdSAML, err)
	}

	gotSAML, err := c.GetSAMLProvider(ctx, &iam.GetSAMLProviderInput{SAMLProviderArn: createdSAML.SAMLProviderArn})
	if err != nil || aws.ToString(gotSAML.SAMLMetadataDocument) != samlXML ||
		gotSAML.ValidUntil.UTC().Year() != 2035 || aws.ToString(gotSAML.SAMLProviderUUID) == "" {
		t.Fatalf("get saml: %+v %v", gotSAML, err)
	}

	updatedXML := `<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" validUntil="2036-06-01T00:00:00Z"/>`
	if _, err = c.UpdateSAMLProvider(ctx, &iam.UpdateSAMLProviderInput{
		SAMLProviderArn: createdSAML.SAMLProviderArn, SAMLMetadataDocument: new(updatedXML),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.TagSAMLProvider(ctx, &iam.TagSAMLProviderInput{
		SAMLProviderArn: createdSAML.SAMLProviderArn,
		Tags:            []iamtypes.Tag{{Key: new("env"), Value: new("prod")}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.UntagSAMLProvider(ctx, &iam.UntagSAMLProviderInput{
		SAMLProviderArn: createdSAML.SAMLProviderArn, TagKeys: []string{"team"},
	}); err != nil {
		t.Fatal(err)
	}

	samlTags, err := c.ListSAMLProviderTags(ctx, &iam.ListSAMLProviderTagsInput{
		SAMLProviderArn: createdSAML.SAMLProviderArn,
	})
	if err != nil || len(samlTags.Tags) != 1 || aws.ToString(samlTags.Tags[0].Key) != "env" {
		t.Fatalf("saml tags: %+v %v", samlTags, err)
	}

	samlList, err := c.ListSAMLProviders(ctx, &iam.ListSAMLProvidersInput{})
	if err != nil || len(samlList.SAMLProviderList) != 1 {
		t.Fatalf("list saml: %+v %v", samlList, err)
	}

	// OIDC provider tagging operations.
	oidcOut, err := c.CreateOpenIDConnectProvider(ctx, &iam.CreateOpenIDConnectProviderInput{
		Url: new("https://issuer.example.com"), ClientIDList: []string{"aud"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.TagOpenIDConnectProvider(ctx, &iam.TagOpenIDConnectProviderInput{
		OpenIDConnectProviderArn: oidcOut.OpenIDConnectProviderArn,
		Tags: []iamtypes.Tag{
			{Key: new("k1"), Value: new("v1")},
			{Key: new("k2"), Value: new("v2")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.UntagOpenIDConnectProvider(ctx, &iam.UntagOpenIDConnectProviderInput{
		OpenIDConnectProviderArn: oidcOut.OpenIDConnectProviderArn, TagKeys: []string{"k1"},
	}); err != nil {
		t.Fatal(err)
	}

	oidcTags, err := c.ListOpenIDConnectProviderTags(ctx, &iam.ListOpenIDConnectProviderTagsInput{
		OpenIDConnectProviderArn: oidcOut.OpenIDConnectProviderArn,
	})
	if err != nil || len(oidcTags.Tags) != 1 || aws.ToString(oidcTags.Tags[0].Key) != "k2" {
		t.Fatalf("oidc tags: %+v %v", oidcTags, err)
	}

	// Virtual MFA devices.
	if _, err = c.CreateUser(ctx, &iam.CreateUserInput{UserName: new("mfa-user")}); err != nil {
		t.Fatal(err)
	}

	vMFA, err := c.CreateVirtualMFADevice(ctx, &iam.CreateVirtualMFADeviceInput{
		VirtualMFADeviceName: new("phone"), Path: new("/ops/"),
		Tags: []iamtypes.Tag{{Key: new("owner"), Value: new("ops")}},
	})
	if err != nil || aws.ToString(vMFA.VirtualMFADevice.SerialNumber) != "arn:aws:iam::000000000000:mfa/ops/phone" ||
		len(vMFA.VirtualMFADevice.Base32StringSeed) == 0 {
		t.Fatalf("create virtual mfa: %+v %v", vMFA, err)
	}

	serial := vMFA.VirtualMFADevice.SerialNumber
	// Resource Groups Tagging API integration on SAML, OIDC, and MFA.
	tagRes, err := tagAPI.TagResources(ctx, &resourcegroupstaggingapi.TagResourcesInput{
		ResourceARNList: []string{*createdSAML.SAMLProviderArn, *oidcOut.OpenIDConnectProviderArn, *serial},
		Tags:            map[string]string{"rgt": "ok"},
	})
	if err != nil || len(tagRes.FailedResourcesMap) != 0 {
		t.Fatalf("rgt tag resources: %+v %v", tagRes, err)
	}

	// Invalid TOTP code rejected.
	_, err = c.EnableMFADevice(ctx, &iam.EnableMFADeviceInput{
		UserName: new("mfa-user"), SerialNumber: serial,
		AuthenticationCode1: new("000000"), AuthenticationCode2: new("111111"),
	})
	wantCode(t, err, "InvalidAuthenticationCode")

	step := time.Now().Unix() / 30
	if _, err = c.EnableMFADevice(ctx, &iam.EnableMFADeviceInput{
		UserName: new("mfa-user"), SerialNumber: serial,
		AuthenticationCode1: new(testTOTPCode(t, vMFA.VirtualMFADevice.Base32StringSeed, step)),
		AuthenticationCode2: new(testTOTPCode(t, vMFA.VirtualMFADevice.Base32StringSeed, step+1)),
	}); err != nil {
		t.Fatal(err)
	}

	// Resync with drifted codes.
	if _, err = c.ResyncMFADevice(ctx, &iam.ResyncMFADeviceInput{
		UserName: new("mfa-user"), SerialNumber: serial,
		AuthenticationCode1: new(testTOTPCode(t, vMFA.VirtualMFADevice.Base32StringSeed, step+6)),
		AuthenticationCode2: new(testTOTPCode(t, vMFA.VirtualMFADevice.Base32StringSeed, step+7)),
	}); err != nil {
		t.Fatal(err)
	}

	gotMFA, err := c.GetMFADevice(ctx, &iam.GetMFADeviceInput{SerialNumber: serial, UserName: new("mfa-user")})
	if err != nil || aws.ToString(gotMFA.UserName) != "mfa-user" || gotMFA.EnableDate == nil {
		t.Fatalf("get mfa: %+v %v", gotMFA, err)
	}

	userMFAs, err := c.ListMFADevices(ctx, &iam.ListMFADevicesInput{UserName: new("mfa-user")})
	if err != nil || len(userMFAs.MFADevices) != 1 {
		t.Fatalf("list mfa: %+v %v", userMFAs, err)
	}

	assignedList, err := c.ListVirtualMFADevices(ctx, &iam.ListVirtualMFADevicesInput{
		AssignmentStatus: iamtypes.AssignmentStatusTypeAssigned,
	})
	if err != nil || len(assignedList.VirtualMFADevices) != 1 ||
		len(assignedList.VirtualMFADevices[0].Base32StringSeed) != 0 ||
		assignedList.VirtualMFADevices[0].User == nil {
		t.Fatalf("list virtual mfa assigned: %+v %v", assignedList, err)
	}

	unassignedList, err := c.ListVirtualMFADevices(ctx, &iam.ListVirtualMFADevicesInput{
		AssignmentStatus: iamtypes.AssignmentStatusTypeUnassigned,
	})
	if err != nil || len(unassignedList.VirtualMFADevices) != 0 {
		t.Fatalf("list virtual mfa unassigned: %+v %v", unassignedList, err)
	}

	mfaTags, err := c.ListMFADeviceTags(ctx, &iam.ListMFADeviceTagsInput{SerialNumber: serial})
	if err != nil || len(mfaTags.Tags) != 2 {
		t.Fatalf("mfa tags: %+v %v", mfaTags, err)
	}

	// Cannot delete assigned device or user with assigned MFA device.
	_, err = c.DeleteVirtualMFADevice(ctx, &iam.DeleteVirtualMFADeviceInput{SerialNumber: serial})
	wantCode(t, err, "DeleteConflict")
	_, err = c.DeleteUser(ctx, &iam.DeleteUserInput{UserName: new("mfa-user")})
	wantCode(t, err, "DeleteConflict")

	if _, err = c.DeactivateMFADevice(ctx, &iam.DeactivateMFADeviceInput{
		UserName: new("mfa-user"), SerialNumber: serial,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteVirtualMFADevice(ctx, &iam.DeleteVirtualMFADeviceInput{SerialNumber: serial}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteUser(ctx, &iam.DeleteUserInput{UserName: new("mfa-user")}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteSAMLProvider(ctx, &iam.DeleteSAMLProviderInput{
		SAMLProviderArn: createdSAML.SAMLProviderArn,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIAMServerCertificatesSSHKeysAndServiceCredentials(t *testing.T) {
	s := server(t)
	c := iamClient(s, "test", "us-east-1")
	ctx := context.Background()

	// Server certificates.
	expiry := time.Now().UTC().Add(365 * 24 * time.Hour).Truncate(time.Second)
	cert1, key1 := testSelfSignedCert(t, expiry)
	_, key2 := testSelfSignedCert(t, expiry)

	_, err := c.UploadServerCertificate(ctx, &iam.UploadServerCertificateInput{
		ServerCertificateName: new("bad-pair"), CertificateBody: new(cert1), PrivateKey: new(key2),
	})
	wantCode(t, err, "KeyPairMismatch")

	_, err = c.UploadServerCertificate(ctx, &iam.UploadServerCertificateInput{
		ServerCertificateName: new("bad-cert"), CertificateBody: new("not-pem"), PrivateKey: new(key1),
	})
	wantCode(t, err, "MalformedCertificate")

	uploaded, err := c.UploadServerCertificate(ctx, &iam.UploadServerCertificateInput{
		ServerCertificateName: new("tls-cert"),
		Path:                  new("/cloudfront/"),
		CertificateBody:       new(cert1),
		PrivateKey:            new(key1),
		CertificateChain:      new(cert1),
		Tags:                  []iamtypes.Tag{{Key: new("svc"), Value: new("web")}},
	})
	if err != nil || !uploaded.ServerCertificateMetadata.Expiration.UTC().Equal(expiry) ||
		aws.ToString(
			uploaded.ServerCertificateMetadata.Arn,
		) != "arn:aws:iam::000000000000:server-certificate/cloudfront/tls-cert" {
		t.Fatalf("upload cert: %+v %v", uploaded, err)
	}

	if _, err = c.UpdateServerCertificate(ctx, &iam.UpdateServerCertificateInput{
		ServerCertificateName:    new("tls-cert"),
		NewServerCertificateName: new("tls-cert-v2"),
		NewPath:                  new("/alb/"),
	}); err != nil {
		t.Fatal(err)
	}

	gotCert, err := c.GetServerCertificate(ctx, &iam.GetServerCertificateInput{
		ServerCertificateName: new("tls-cert-v2"),
	})
	if err != nil || aws.ToString(gotCert.ServerCertificate.ServerCertificateMetadata.Path) != "/alb/" ||
		aws.ToString(gotCert.ServerCertificate.CertificateBody) != cert1 {
		t.Fatalf("get cert: %+v %v", gotCert, err)
	}

	if _, err = c.TagServerCertificate(ctx, &iam.TagServerCertificateInput{
		ServerCertificateName: new("tls-cert-v2"),
		Tags:                  []iamtypes.Tag{{Key: new("env"), Value: new("prod")}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.UntagServerCertificate(ctx, &iam.UntagServerCertificateInput{
		ServerCertificateName: new("tls-cert-v2"), TagKeys: []string{"svc"},
	}); err != nil {
		t.Fatal(err)
	}

	certTags, err := c.ListServerCertificateTags(ctx, &iam.ListServerCertificateTagsInput{
		ServerCertificateName: new("tls-cert-v2"),
	})
	if err != nil || len(certTags.Tags) != 1 || aws.ToString(certTags.Tags[0].Key) != "env" {
		t.Fatalf("cert tags: %+v %v", certTags, err)
	}

	certList, err := c.ListServerCertificates(ctx, &iam.ListServerCertificatesInput{PathPrefix: new("/alb/")})
	if err != nil || len(certList.ServerCertificateMetadataList) != 1 {
		t.Fatalf("list certs: %+v %v", certList, err)
	}

	if _, err = c.DeleteServerCertificate(ctx, &iam.DeleteServerCertificateInput{
		ServerCertificateName: new("tls-cert-v2"),
	}); err != nil {
		t.Fatal(err)
	}

	// SSH public keys.
	if _, err = c.CreateUser(ctx, &iam.CreateUserInput{UserName: new("dev")}); err != nil {
		t.Fatal(err)
	}

	pubKey := testSSHPublicKey(t)
	_, err = c.UploadSSHPublicKey(ctx, &iam.UploadSSHPublicKeyInput{
		UserName: new("dev"), SSHPublicKeyBody: new("not-an-ssh-key"),
	})
	wantCode(t, err, "InvalidPublicKey")

	sshOut, err := c.UploadSSHPublicKey(ctx, &iam.UploadSSHPublicKeyInput{
		UserName: new("dev"), SSHPublicKeyBody: new(pubKey),
	})
	if err != nil || !strings.HasPrefix(aws.ToString(sshOut.SSHPublicKey.SSHPublicKeyId), "APKA") ||
		aws.ToString(sshOut.SSHPublicKey.Fingerprint) == "" {
		t.Fatalf("upload ssh key: %+v %v", sshOut, err)
	}

	_, err = c.UploadSSHPublicKey(ctx, &iam.UploadSSHPublicKeyInput{
		UserName: new("dev"), SSHPublicKeyBody: new(pubKey),
	})
	wantCode(t, err, "DuplicateSSHPublicKey")

	gotPEM, err := c.GetSSHPublicKey(ctx, &iam.GetSSHPublicKeyInput{
		UserName: new("dev"), SSHPublicKeyId: sshOut.SSHPublicKey.SSHPublicKeyId,
		Encoding: iamtypes.EncodingTypePem,
	})
	if err != nil ||
		!strings.HasPrefix(aws.ToString(gotPEM.SSHPublicKey.SSHPublicKeyBody), "-----BEGIN PUBLIC KEY-----") {
		t.Fatalf("get ssh pem: %+v %v", gotPEM, err)
	}

	if _, err = c.UpdateSSHPublicKey(ctx, &iam.UpdateSSHPublicKeyInput{
		UserName: new("dev"), SSHPublicKeyId: sshOut.SSHPublicKey.SSHPublicKeyId,
		Status: iamtypes.StatusTypeInactive,
	}); err != nil {
		t.Fatal(err)
	}

	sshList, err := c.ListSSHPublicKeys(ctx, &iam.ListSSHPublicKeysInput{UserName: new("dev")})
	if err != nil || len(sshList.SSHPublicKeys) != 1 || sshList.SSHPublicKeys[0].Status != iamtypes.StatusTypeInactive {
		t.Fatalf("list ssh keys: %+v %v", sshList, err)
	}

	// Service-specific credentials.
	_, err = c.CreateServiceSpecificCredential(ctx, &iam.CreateServiceSpecificCredentialInput{
		UserName: new("dev"), ServiceName: new("unknown.amazonaws.com"),
	})
	wantCode(t, err, "NotSupportedService")

	credOut, err := c.CreateServiceSpecificCredential(ctx, &iam.CreateServiceSpecificCredentialInput{
		UserName: new("dev"), ServiceName: new("codecommit.amazonaws.com"), CredentialAgeDays: aws.Int32(30),
	})
	if err != nil || credOut.ServiceSpecificCredential.ExpirationDate == nil ||
		aws.ToString(credOut.ServiceSpecificCredential.ServicePassword) == "" {
		t.Fatalf("create service cred: %+v %v", credOut, err)
	}

	credID := credOut.ServiceSpecificCredential.ServiceSpecificCredentialId
	oldPass := aws.ToString(credOut.ServiceSpecificCredential.ServicePassword)

	resetOut, err := c.ResetServiceSpecificCredential(ctx, &iam.ResetServiceSpecificCredentialInput{
		UserName: new("dev"), ServiceSpecificCredentialId: credID,
	})
	if err != nil || aws.ToString(resetOut.ServiceSpecificCredential.ServicePassword) == oldPass {
		t.Fatalf("reset service cred: %+v %v", resetOut, err)
	}

	if _, err = c.UpdateServiceSpecificCredential(ctx, &iam.UpdateServiceSpecificCredentialInput{
		UserName: new("dev"), ServiceSpecificCredentialId: credID, Status: iamtypes.StatusTypeInactive,
	}); err != nil {
		t.Fatal(err)
	}

	credList, err := c.ListServiceSpecificCredentials(ctx, &iam.ListServiceSpecificCredentialsInput{
		AllUsers: aws.Bool(true), ServiceName: new("codecommit.amazonaws.com"),
	})
	if err != nil || len(credList.ServiceSpecificCredentials) != 1 ||
		credList.ServiceSpecificCredentials[0].Status != iamtypes.StatusTypeInactive {
		t.Fatalf("list service creds: %+v %v", credList, err)
	}

	// DeleteUser blocked by SSH key and service-specific credential.
	_, err = c.DeleteUser(ctx, &iam.DeleteUserInput{UserName: new("dev")})
	wantCode(t, err, "DeleteConflict")

	if _, err = c.DeleteSSHPublicKey(ctx, &iam.DeleteSSHPublicKeyInput{
		UserName: new("dev"), SSHPublicKeyId: sshOut.SSHPublicKey.SSHPublicKeyId,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteUser(ctx, &iam.DeleteUserInput{UserName: new("dev")})
	wantCode(t, err, "DeleteConflict")

	if _, err = c.DeleteServiceSpecificCredential(ctx, &iam.DeleteServiceSpecificCredentialInput{
		UserName: new("dev"), ServiceSpecificCredentialId: credID,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteUser(ctx, &iam.DeleteUserInput{UserName: new("dev")}); err != nil {
		t.Fatal(err)
	}
}
