package integration_test

import (
	"context"
	"testing"

	iam "github.com/aws/aws-sdk-go-v2/service/iam"
	kms "github.com/aws/aws-sdk-go-v2/service/kms"
	data "github.com/aws/aws-sdk-go-v2/service/redshiftdata"
	serverless "github.com/aws/aws-sdk-go-v2/service/redshiftserverless"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	secrets "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

func TestRedshiftServerlessManagedReferences(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := serverlessClient(s.URL)
	keys := kms.New(kms.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	key, err := keys.CreateKey(ctx, &kms.CreateKeyInput{})
	if err != nil {
		t.Fatal(err)
	}

	roles := iam.New(iam.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	role, err := roles.CreateRole(
		ctx,
		&iam.CreateRoleInput{
			RoleName: new("warehouse"),
			AssumeRolePolicyDocument: new(
				`{"Version":"2012-10-17",
"Statement":[{"Effect":"Allow",
"Principal":{"Service":"redshift.amazonaws.com"},
"Action":"sts:AssumeRole"}]}`,
			),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	n, err := c.CreateNamespace(
		ctx,
		&serverless.CreateNamespaceInput{
			NamespaceName:               new("managed"),
			AdminUsername:               new("admin"),
			ManageAdminPassword:         new(true),
			AdminPasswordSecretKmsKeyId: key.KeyMetadata.KeyId,
			KmsKeyId:                    key.KeyMetadata.KeyId,
			IamRoles:                    []string{*role.Role.Arn},
			DefaultIamRoleArn:           role.Role.Arn,
		},
	)
	if err != nil || n.Namespace.AdminPasswordSecretArn == nil {
		t.Fatal(n, err)
	}

	secretClient := secrets.New(
		secrets.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	_, err = secretClient.GetSecretValue(
		ctx,
		&secrets.GetSecretValueInput{SecretId: n.Namespace.AdminPasswordSecretArn},
	)
	if err != nil {
		t.Fatal(err)
	}

	w, err := c.CreateWorkgroup(
		ctx,
		&serverless.CreateWorkgroupInput{NamespaceName: new("managed"), WorkgroupName: new("managed")},
	)
	if err != nil {
		t.Fatal(err)
	}

	waitServerless(t, c, w.Workgroup.WorkgroupName)
	tags := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := tags.TagResources(
		ctx,
		&rgta.TagResourcesInput{
			ResourceARNList: []string{*w.Workgroup.WorkgroupArn},
			Tags:            map[string]string{"team": "platform"},
		},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatal(tagged, err)
	}

	_, err = c.UpdateWorkgroup(
		ctx,
		&serverless.UpdateWorkgroupInput{
			WorkgroupName:    w.Workgroup.WorkgroupName,
			SecurityGroupIds: []string{"sg-deadbeef"},
		},
	)
	wantAccessAnalyzerError(t, err, "ValidationException")
	putRedshiftFixture(
		t,
		s.URL,
		"managed",
		`{"contains":"select managed","columnMetadata":[{"name":"answer"}],"records":[[{"stringValue":"managed"}]]}`,
	)
	dc := redshiftDataClient(s.URL)
	out, err := dc.ExecuteStatement(
		ctx,
		&data.ExecuteStatementInput{
			WorkgroupName: w.Workgroup.WorkgroupArn,
			SecretArn:     n.Namespace.AdminPasswordSecretArn,
			Database:      new("dev"),
			Sql:           new("select managed"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	waitRedshiftStatement(t, dc, out.Id, "FINISHED")
}
