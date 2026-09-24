package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/smithy-go"
)

const trustPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
	`"Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}`

const s3ReadPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`

func iamClient(s *httptest.Server, key, region string) *iam.Client {
	return iam.New(iam.Options{
		Region:       region,
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// decoded URL-decodes a policy document, as IAM returns them encoded.
func decoded(t *testing.T, doc *string) string {
	t.Helper()
	out, err := url.QueryUnescape(aws.ToString(doc))
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// wantCode fails unless err is an API error with code.
func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}

func TestIAMRoles(t *testing.T) {
	s := server(t)
	c := iamClient(s, "test", "us-east-1")
	ctx := context.Background()

	created, err := c.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 aws.String("worker"),
		Path:                     aws.String("/service/"),
		AssumeRolePolicyDocument: aws.String(trustPolicy),
		Description:              aws.String("runs jobs"),
		Tags:                     []iamtypes.Tag{{Key: aws.String("team"), Value: aws.String("core")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	role := created.Role
	if *role.Arn != "arn:aws:iam::000000000000:role/service/worker" || len(*role.RoleId) != 20 ||
		role.CreateDate.IsZero() || *role.MaxSessionDuration != 3600 {
		t.Fatalf("created %+v", role)
	}

	_, err = c.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: aws.String("WORKER"), AssumeRolePolicyDocument: aws.String(trustPolicy),
	})
	wantCode(t, err, "EntityAlreadyExists")

	_, err = c.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: aws.String("broken"), AssumeRolePolicyDocument: aws.String("{not json"),
	})
	wantCode(t, err, "MalformedPolicyDocument")

	// IAM is global: a client in another region sees the same role.
	got, err := iamClient(s, "test", "eu-west-1").GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String("Worker")})
	if err != nil || decoded(t, got.Role.AssumeRolePolicyDocument) != trustPolicy ||
		*got.Role.Description != "runs jobs" || len(got.Role.Tags) != 1 {
		t.Fatal(got, err)
	}

	// Another account does not.
	_, err = iamClient(s, "team-b", "us-east-1").GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String("worker")})
	wantCode(t, err, "NoSuchEntity")

	if _, err = c.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: aws.String("api"), AssumeRolePolicyDocument: aws.String(trustPolicy),
	}); err != nil {
		t.Fatal(err)
	}

	first, err := c.ListRoles(ctx, &iam.ListRolesInput{MaxItems: aws.Int32(1)})
	if err != nil || len(first.Roles) != 1 || *first.Roles[0].RoleName != "api" || !first.IsTruncated {
		t.Fatal(first, err)
	}

	second, err := c.ListRoles(ctx, &iam.ListRolesInput{Marker: first.Marker})
	if err != nil || len(second.Roles) != 1 || *second.Roles[0].RoleName != "worker" || second.IsTruncated {
		t.Fatal(second, err)
	}

	if filtered, listErr := c.ListRoles(
		ctx,
		&iam.ListRolesInput{PathPrefix: aws.String("/service/")},
	); listErr != nil ||
		len(filtered.Roles) != 1 {
		t.Fatal(filtered, listErr)
	}

	_, err = c.UpdateRole(
		ctx,
		&iam.UpdateRoleInput{RoleName: aws.String("worker"), MaxSessionDuration: aws.Int32(7200)},
	)
	if err != nil {
		t.Fatal(err)
	}

	described, err := c.UpdateRoleDescription(ctx, &iam.UpdateRoleDescriptionInput{
		RoleName: aws.String("worker"), Description: aws.String("runs batch jobs"),
	})
	if err != nil || *described.Role.Description != "runs batch jobs" {
		t.Fatal(described, err)
	}

	_, err = c.UpdateAssumeRolePolicy(ctx, &iam.UpdateAssumeRolePolicyInput{
		RoleName: aws.String("worker"), PolicyDocument: aws.String(s3ReadPolicy),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.TagRole(ctx, &iam.TagRoleInput{
		RoleName: aws.String("worker"), Tags: []iamtypes.Tag{{Key: aws.String("env"), Value: aws.String("dev")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UntagRole(ctx, &iam.UntagRoleInput{RoleName: aws.String("worker"), TagKeys: []string{"team"}})
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListRoleTags(ctx, &iam.ListRoleTagsInput{RoleName: aws.String("worker")})
	if err != nil || len(tags.Tags) != 1 || *tags.Tags[0].Key != "env" {
		t.Fatal(tags, err)
	}

	got, err = c.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String("worker")})
	if err != nil || *got.Role.MaxSessionDuration != 7200 ||
		decoded(t, got.Role.AssumeRolePolicyDocument) != s3ReadPolicy {
		t.Fatal(got, err)
	}

	_, err = c.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName: aws.String("worker"), PolicyName: aws.String("read"), PolicyDocument: aws.String(s3ReadPolicy),
	})
	if err != nil {
		t.Fatal(err)
	}

	inline, err := c.GetRolePolicy(ctx, &iam.GetRolePolicyInput{
		RoleName: aws.String("worker"), PolicyName: aws.String("READ"),
	})
	if err != nil || *inline.PolicyName != "read" || decoded(t, inline.PolicyDocument) != s3ReadPolicy {
		t.Fatal(inline, err)
	}

	names, err := c.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{RoleName: aws.String("worker")})
	if err != nil || len(names.PolicyNames) != 1 {
		t.Fatal(names, err)
	}

	_, err = c.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String("worker")})
	wantCode(t, err, "DeleteConflict")

	_, err = c.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{
		RoleName: aws.String("worker"), PolicyName: aws.String("read"),
	})
	if err != nil {
		t.Fatal(err)
	}

	profiles, err := c.ListInstanceProfilesForRole(ctx, &iam.ListInstanceProfilesForRoleInput{
		RoleName: aws.String("worker"),
	})
	if err != nil || len(profiles.InstanceProfiles) != 0 {
		t.Fatal(profiles, err)
	}

	if _, err = c.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String("worker")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String("worker")})
	wantCode(t, err, "NoSuchEntity")
}

func TestIAMManagedPolicies(t *testing.T) {
	s := server(t)
	c := iamClient(s, "test", "us-east-1")
	ctx := context.Background()

	if _, err := c.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: aws.String("fn"), AssumeRolePolicyDocument: aws.String(trustPolicy),
	}); err != nil {
		t.Fatal(err)
	}

	created, err := c.CreatePolicy(ctx, &iam.CreatePolicyInput{
		PolicyName: aws.String("read"), Path: aws.String("/app/"), PolicyDocument: aws.String(s3ReadPolicy),
	})
	if err != nil || *created.Policy.Arn != "arn:aws:iam::000000000000:policy/app/read" ||
		*created.Policy.DefaultVersionId != "v1" {
		t.Fatal(created, err)
	}

	arn := created.Policy.Arn
	basic := aws.String("arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole")
	for _, policyARN := range []*string{arn, arn, basic} {
		if _, err = c.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
			RoleName: aws.String("fn"), PolicyArn: policyARN,
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
		RoleName: aws.String("fn"), PolicyArn: aws.String("arn:aws:iam::000000000000:policy/missing"),
	})
	wantCode(t, err, "NoSuchEntity")

	attached, err := c.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String("fn")})
	if err != nil || len(attached.AttachedPolicies) != 2 {
		t.Fatal(attached, err)
	}

	onlyApp, err := c.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{
		RoleName: aws.String("fn"), PathPrefix: aws.String("/app/"),
	})
	if err != nil || len(onlyApp.AttachedPolicies) != 1 || *onlyApp.AttachedPolicies[0].PolicyName != "read" {
		t.Fatal(onlyApp, err)
	}

	got, err := c.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: arn})
	if err != nil || *got.Policy.AttachmentCount != 1 {
		t.Fatal(got, err)
	}

	listed, err := c.ListPolicies(ctx, &iam.ListPoliciesInput{Scope: iamtypes.PolicyScopeTypeLocal, OnlyAttached: true})
	if err != nil || len(listed.Policies) != 1 {
		t.Fatal(listed, err)
	}

	_, err = c.DeletePolicy(ctx, &iam.DeletePolicyInput{PolicyArn: arn})
	wantCode(t, err, "DeleteConflict")

	v2, err := c.CreatePolicyVersion(ctx, &iam.CreatePolicyVersionInput{
		PolicyArn: arn, PolicyDocument: aws.String(trustPolicy), SetAsDefault: true,
	})
	if err != nil || *v2.PolicyVersion.VersionId != "v2" || !v2.PolicyVersion.IsDefaultVersion {
		t.Fatal(v2, err)
	}

	version, err := c.GetPolicyVersion(ctx, &iam.GetPolicyVersionInput{PolicyArn: arn, VersionId: aws.String("v2")})
	if err != nil || decoded(t, version.PolicyVersion.Document) != trustPolicy {
		t.Fatal(version, err)
	}

	_, err = c.DeletePolicyVersion(ctx, &iam.DeletePolicyVersionInput{PolicyArn: arn, VersionId: aws.String("v2")})
	wantCode(t, err, "DeleteConflict")

	for range 3 {
		if _, err = c.CreatePolicyVersion(ctx, &iam.CreatePolicyVersionInput{
			PolicyArn: arn, PolicyDocument: aws.String(s3ReadPolicy),
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.CreatePolicyVersion(ctx, &iam.CreatePolicyVersionInput{
		PolicyArn: arn, PolicyDocument: aws.String(s3ReadPolicy),
	})
	wantCode(t, err, "LimitExceeded")

	versions, err := c.ListPolicyVersions(ctx, &iam.ListPolicyVersionsInput{PolicyArn: arn})
	if err != nil || len(versions.Versions) != 5 {
		t.Fatal(versions, err)
	}

	for _, v := range versions.Versions {
		if v.IsDefaultVersion {
			continue
		}

		if _, err = c.DeletePolicyVersion(ctx, &iam.DeletePolicyVersionInput{
			PolicyArn: arn, VersionId: v.VersionId,
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, policyARN := range []*string{arn, basic} {
		if _, err = c.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{
			RoleName: aws.String("fn"), PolicyArn: policyARN,
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{RoleName: aws.String("fn"), PolicyArn: arn})
	wantCode(t, err, "NoSuchEntity")

	if _, err = c.DeletePolicy(ctx, &iam.DeletePolicyInput{PolicyArn: arn}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: arn})
	wantCode(t, err, "NoSuchEntity")
}

func TestIAMPersists(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := iamClient(s, "test", "us-east-1")
	if _, err := c.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: aws.String("fn"), AssumeRolePolicyDocument: aws.String(trustPolicy),
	}); err != nil {
		t.Fatal(err)
	}

	created, err := c.CreatePolicy(ctx, &iam.CreatePolicyInput{
		PolicyName: aws.String("read"), PolicyDocument: aws.String(s3ReadPolicy),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.AttachRolePolicy(
		ctx,
		&iam.AttachRolePolicyInput{RoleName: aws.String("fn"), PolicyArn: created.Policy.Arn},
	)
	if err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = iamClient(s, "test", "eu-west-1")
	attached, err := c.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String("fn")})
	if err != nil || len(attached.AttachedPolicies) != 1 ||
		*attached.AttachedPolicies[0].PolicyArn != *created.Policy.Arn {
		t.Fatal(attached, err)
	}

	got, err := c.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: created.Policy.Arn})
	if err != nil || *got.Policy.AttachmentCount != 1 {
		t.Fatal(got, err)
	}
}
