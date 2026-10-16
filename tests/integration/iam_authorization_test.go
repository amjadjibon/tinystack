package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/gateway"
)

func authorizationServer(t *testing.T, mode string, verify bool) *httptest.Server {
	t.Helper()
	cfg := testConfig()
	cfg.Identity.Accounts["test"] = cfg.Defaults.Account
	cfg.Identity.Authorization = mode
	if verify {
		cfg.Identity.Mode = config.IdentityVerify
		cfg.Identity.SecretKeys = map[string]string{"test": "test"}
	}

	return serverConfig(t, cfg, &fakeRuntime{})
}

func authorizationUser(t *testing.T, s *httptest.Server) *iamtypes.AccessKey {
	t.Helper()
	root := iamClient(s, "test", "us-east-1")
	if _, err := root.CreateUser(
		context.Background(),
		&iam.CreateUserInput{UserName: new("alice"), Path: new("/apps/")},
	); err != nil {
		t.Fatal(err)
	}

	issued, err := root.CreateAccessKey(context.Background(), &iam.CreateAccessKeyInput{UserName: new("alice")})
	if err != nil {
		t.Fatal(err)
	}

	return issued.AccessKey
}

func authorizationPolicy(t *testing.T, s *httptest.Server, document string) {
	t.Helper()
	_, err := iamClient(s, "test", "us-east-1").PutUserPolicy(context.Background(), &iam.PutUserPolicyInput{
		UserName: new("alice"), PolicyName: new("permissions"), PolicyDocument: new(document),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func authorizationProvider(key *iamtypes.AccessKey) aws.CredentialsProvider {
	return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{
			AccessKeyID:     aws.ToString(key.AccessKeyId),
			SecretAccessKey: aws.ToString(key.SecretAccessKey),
		}, nil
	})
}

func authorizationS3(s *httptest.Server, key *iamtypes.AccessKey) *s3.Client {
	return s3Client(
		s,
		aws.ToString(key.AccessKeyId),
		"us-east-1",
		func(o *s3.Options) { o.Credentials = authorizationProvider(key) },
	)
}

func mustAuthorize(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestIAMAuthorizationS3(t *testing.T) {
	for _, verify := range []bool{false, true} {
		t.Run(fmt.Sprintf("verify=%t", verify), func(t *testing.T) {
			s := authorizationServer(t, config.AuthorizationEnforce, verify)
			ctx := context.Background()
			root := s3Client(s, "test", "us-east-1")
			_, err := root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("iam-bucket")})
			mustAuthorize(t, err)
			_, err = root.PutObject(
				ctx,
				&s3.PutObjectInput{Bucket: new("iam-bucket"), Key: new("alice/read"), Body: strings.NewReader("data")},
			)
			mustAuthorize(t, err)
			key := authorizationUser(t, s)
			user := authorizationS3(s, key)
			_, err = user.GetObject(ctx, &s3.GetObjectInput{Bucket: new("iam-bucket"), Key: new("alice/read")})
			wantCode(t, err, "AccessDenied")
			if !strings.Contains(err.Error(), "arn:aws:iam::000000000000:user/apps/alice") ||
				!strings.Contains(err.Error(), "no identity-based policy allows the s3:GetObject action") {
				t.Fatal(err)
			}

			authorizationPolicy(t, s, `{"Version":"2012-10-17",
"Statement":[
			{"Sid":"read",
"Effect":"Allow",
"Action":"s3:GetObject",
"Resource":"arn:aws:s3:::iam-bucket/${aws:username}/*"},
			{"Effect":"Allow",
"Action":"s3:ListBucket",
"Resource":"arn:aws:s3:::iam-bucket"}]}`)
			got, err := user.GetObject(ctx, &s3.GetObjectInput{Bucket: new("iam-bucket"), Key: new("alice/read")})
			mustAuthorize(t, err)
			got.Body.Close()
			_, err = user.HeadObject(ctx, &s3.HeadObjectInput{Bucket: new("iam-bucket"), Key: new("alice/read")})
			mustAuthorize(t, err)
			_, err = user.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: new("iam-bucket")})
			mustAuthorize(t, err)
			_, err = user.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: new("iam-bucket")})
			mustAuthorize(t, err)
			_, err = user.PutObject(
				ctx,
				&s3.PutObjectInput{Bucket: new("iam-bucket"), Key: new("alice/write"), Body: strings.NewReader("no")},
			)
			wantCode(t, err, "AccessDenied")

			authorizationPolicy(t, s, `{"Statement":[{"Effect":"Allow",
"Action":"s3:*",
"Resource":"*"},
			{"Sid":"block-write",
"Effect":"Deny",
"Action":"s3:PutObject",
"Resource":"arn:aws:s3:::iam-bucket/*"}]}`)
			_, err = user.PutObject(
				ctx,
				&s3.PutObjectInput{Bucket: new("iam-bucket"), Key: new("alice/write"), Body: strings.NewReader("no")},
			)
			wantCode(t, err, "AccessDenied")
			if !strings.Contains(err.Error(), "explicit deny in an identity-based policy") {
				t.Fatal(err)
			}

			authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow","NotAction":"s3:Delete*","Resource":"*"}}`)
			_, err = user.PutObject(
				ctx,
				&s3.PutObjectInput{Bucket: new("iam-bucket"), Key: new("alice/write"), Body: strings.NewReader("yes")},
			)
			mustAuthorize(t, err)
			_, err = user.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: new("iam-bucket"), Key: new("alice/write")})
			wantCode(t, err, "AccessDenied")

			authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow",
"Action":"s3:GetObject",
"Resource":"*", "Condition":{"StringEqualsIfExists":{"aws:Unsupported":"x"}}}}`)
			_, err = user.GetObject(ctx, &s3.GetObjectInput{Bucket: new("iam-bucket"), Key: new("alice/read")})
			wantCode(t, err, "AccessDenied")
			if !strings.Contains(err.Error(), "unsupported condition key aws:Unsupported") {
				t.Fatal(err)
			}

			// Root never consults the user's Deny; unknown keys keep existing semantics.
			_, err = root.ListBuckets(ctx, &s3.ListBucketsInput{})
			mustAuthorize(t, err)
			_, err = s3Client(s, "unknown", "us-east-1").ListBuckets(ctx, &s3.ListBucketsInput{})
			if verify {
				wantCode(t, err, "InvalidAccessKeyId")
			} else {
				mustAuthorize(t, err)
			}
		})
	}
}

func TestIAMAuthorizationModes(t *testing.T) {
	for _, mode := range []string{config.AuthorizationOff, config.AuthorizationSoft} {
		t.Run(mode, func(t *testing.T) {
			s := authorizationServer(t, mode, false)
			key := authorizationUser(t, s)
			_, err := authorizationS3(
				s,
				key,
			).CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: new("mode-bucket")})
			mustAuthorize(t, err)
			resp, err := http.Get(s.URL + "/_tinystack/requests")
			mustAuthorize(t, err)
			defer resp.Body.Close()
			var page struct {
				Items []gateway.Record `json:"requests"`
			}

			mustAuthorize(t, json.NewDecoder(resp.Body).Decode(&page))
			found := false
			for _, record := range page.Items {
				if record.Operation != "CreateBucket" {
					continue
				}

				found = true
				if mode == config.AuthorizationOff && record.Authorization != nil {
					t.Fatalf("off decision: %+v", record)
				}

				if mode == config.AuthorizationSoft && (record.Authorization == nil || record.Authorization.Allowed ||
					!strings.Contains(record.Authorization.Message, "would deny:") || record.Status != http.StatusOK) {
					t.Fatalf("soft decision: %+v", record)
				}
			}

			if !found {
				t.Fatalf("missing CreateBucket: %+v", page)
			}
		})
	}
}

func TestIAMAuthorizationActionAliases(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, false)
	ctx := context.Background()
	root := s3Client(s, "test", "us-east-1")
	_, err := root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("alias-bucket")})
	mustAuthorize(t, err)
	_, err = root.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  new("alias-bucket"),
		VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled},
	})
	mustAuthorize(t, err)
	object, err := root.PutObject(ctx, &s3.PutObjectInput{
		Bucket: new("alias-bucket"), Key: new("versioned"), Body: strings.NewReader("data"),
	})
	mustAuthorize(t, err)
	_, err = root.PutBucketOwnershipControls(ctx, &s3.PutBucketOwnershipControlsInput{
		Bucket: new("alias-bucket"),
		OwnershipControls: &s3types.OwnershipControls{Rules: []s3types.OwnershipControlsRule{
			{ObjectOwnership: s3types.ObjectOwnershipBucketOwnerPreferred},
		}},
	})
	mustAuthorize(t, err)
	topic, err := snsClient(s, "test").CreateTopic(ctx, &sns.CreateTopicInput{Name: new("alias-topic")})
	mustAuthorize(t, err)
	key := authorizationUser(t, s)
	user := authorizationS3(s, key)
	tests := []struct {
		call   func() error
		action string
		code   string
	}{
		{func() error {
			_, err := user.GetObjectAcl(ctx, &s3.GetObjectAclInput{
				Bucket: new("alias-bucket"), Key: new("versioned"), VersionId: object.VersionId,
			})
			return err
		}, "s3:GetObjectVersionAcl", "AccessDenied"},
		{func() error {
			_, err := user.PutObjectAcl(ctx, &s3.PutObjectAclInput{
				Bucket: new("alias-bucket"), Key: new("versioned"), VersionId: object.VersionId,
				ACL: s3types.ObjectCannedACLPrivate,
			})
			return err
		}, "s3:PutObjectVersionAcl", "AccessDenied"},
		{func() error {
			_, err := user.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: new("alias-bucket")})
			return err
		}, "s3:ListBucketVersions", "AccessDenied"},
		{func() error {
			_, err := user.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: new("alias-bucket")})
			return err
		}, "s3:ListBucketMultipartUploads", "AccessDenied"},
		{func() error {
			_, err := user.DeleteBucketOwnershipControls(ctx, &s3.DeleteBucketOwnershipControlsInput{
				Bucket: new("alias-bucket"),
			})
			return err
		}, "s3:PutBucketOwnershipControls", "AccessDenied"},
		{func() error {
			_, err := snsClient(s, aws.ToString(key.AccessKeyId)).PublishBatch(ctx, &sns.PublishBatchInput{
				TopicArn:                   topic.TopicArn,
				PublishBatchRequestEntries: []snstypes.PublishBatchRequestEntry{{Id: new("one"), Message: new("data")}},
			})
			return err
		}, "sns:Publish", "AccessDenied"},
	}

	for _, test := range tests {
		t.Run(test.action, func(t *testing.T) {
			authorizationPolicy(t, s, fmt.Sprintf(
				`{"Statement":{"Effect":"Allow","Action":%q,"Resource":"*"}}`, test.action,
			))
			mustAuthorize(t, test.call())
			authorizationPolicy(t, s, fmt.Sprintf(
				`{"Statement":[{"Effect":"Allow","Action":"*","Resource":"*"},`+
					`{"Effect":"Deny","Action":%q,"Resource":"*"}]}`, test.action,
			))
			wantCode(t, test.call(), test.code)
		})
	}
}

func TestIAMAuthorizationQueuesTablesGroups(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, false)
	ctx := context.Background()
	root := iamClient(s, "test", "us-east-1")
	queue, err := sqsClient(
		s,
		"test",
	).CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: new("iam-queue"), Tags: map[string]string{"team": "blue"}})
	mustAuthorize(t, err)
	_, err = client(s, "test", "us-east-1").CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: new(
			"iam-table",
		),
		KeySchema: []dbtypes.KeySchemaElement{{AttributeName: new("id"), KeyType: dbtypes.KeyTypeHash}},
		AttributeDefinitions: []dbtypes.AttributeDefinition{
			{AttributeName: new("id"), AttributeType: dbtypes.ScalarAttributeTypeS},
		},
		BillingMode: dbtypes.BillingModePayPerRequest,
	})
	mustAuthorize(t, err)
	key := authorizationUser(t, s)
	userQueue := sqsClient(s, aws.ToString(key.AccessKeyId))
	userTable := client(s, aws.ToString(key.AccessKeyId), "us-east-1")
	_, err = userQueue.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: queue.QueueUrl, MessageBody: new("no")})
	wantCode(t, err, "AccessDeniedException")
	_, err = userTable.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: new("iam-table")})
	wantCode(t, err, "AccessDeniedException")
	_, err = root.CreateGroup(ctx, &iam.CreateGroupInput{GroupName: new("readers")})
	mustAuthorize(t, err)
	_, err = root.AddUserToGroup(ctx, &iam.AddUserToGroupInput{GroupName: new("readers"), UserName: new("alice")})
	mustAuthorize(t, err)
	_, err = root.PutGroupPolicy(
		ctx,
		&iam.PutGroupPolicyInput{
			GroupName:  new("readers"),
			PolicyName: new("table"),
			PolicyDocument: new(`{"Statement":{"Effect":"Allow",
"Action":"dynamodb:DescribeTable",
"Resource":"arn:aws:dynamodb:us-east-1:000000000000:table/iam-table"}}`),
		},
	)
	mustAuthorize(t, err)
	_, err = userTable.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: new("iam-table")})
	mustAuthorize(t, err)
	managed, err := root.CreatePolicy(
		ctx,
		&iam.CreatePolicyInput{PolicyName: new("queue"), PolicyDocument: new(`{"Statement":{"Effect":"Allow",
"Action":"sqs:SendMessage",
"Resource":"arn:aws:sqs:us-east-1:000000000000:iam-queue",
"Condition":{"StringEquals":{"aws:ResourceTag/team":"blue"}}}}`)},
	)
	mustAuthorize(t, err)
	_, err = root.AttachGroupPolicy(
		ctx,
		&iam.AttachGroupPolicyInput{GroupName: new("readers"), PolicyArn: managed.Policy.Arn},
	)
	mustAuthorize(t, err)
	_, err = userQueue.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{
		QueueUrl: queue.QueueUrl,
		Entries:  []sqstypes.SendMessageBatchRequestEntry{{Id: new("one"), MessageBody: new("yes")}},
	})
	mustAuthorize(t, err)
	_, err = root.CreatePolicyVersion(ctx, &iam.CreatePolicyVersionInput{
		PolicyArn: managed.Policy.Arn, SetAsDefault: true,
		PolicyDocument: new(`{"Statement":{"Effect":"Deny","Action":"sqs:*","Resource":"*"}}`),
	})
	mustAuthorize(t, err)
	_, err = userQueue.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: queue.QueueUrl, MessageBody: new("no")})
	wantCode(t, err, "AccessDeniedException")
}

func TestIAMAuthorizationSessions(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, true)
	ctx := context.Background()
	root := iamClient(s, "test", "us-east-1")
	key := authorizationUser(t, s)
	role, err := root.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: new("reader"), Path: new("/apps/"),
		AssumeRolePolicyDocument: new(`{"Statement":{"Effect":"Allow",
"Principal":{"AWS":"arn:aws:iam::000000000000:root"},
"Action":"sts:AssumeRole"}}`),
		Tags: []iamtypes.Tag{{Key: new("team"), Value: new("blue")}},
	})
	mustAuthorize(t, err)
	userSTS := sts.New(
		sts.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  authorizationProvider(key),
			Retryer:      aws.NopRetryer{},
		},
	)
	_, err = userSTS.AssumeRole(ctx, &sts.AssumeRoleInput{RoleArn: role.Role.Arn, RoleSessionName: new("session")})
	wantCode(t, err, "AccessDenied")
	authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow",
"Action":"sts:AssumeRole",
"Resource":"arn:aws:iam::000000000000:role/apps/reader"}}`)
	_, err = root.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName: new("reader"), PolicyName: new("read"),
		PolicyDocument: new(`{"Statement":{"Effect":"Allow",
"Action":"s3:ListAllMyBuckets",
"Resource":"*",
"Condition":{"StringEquals":{"aws:PrincipalArn":"arn:aws:iam::000000000000:role/apps/reader",
"aws:PrincipalTag/team":"blue"},
"Null":{"aws:username":"true"},
"StringLike":{"aws:userid":"AROA*:session"}}}}`),
	})
	mustAuthorize(t, err)
	assumed, err := userSTS.AssumeRole(
		ctx,
		&sts.AssumeRoleInput{RoleArn: role.Role.Arn, RoleSessionName: new("session")},
	)
	mustAuthorize(t, err)
	roleS3 := s3Client(
		s,
		"ignored",
		"us-east-1",
		func(o *s3.Options) { o.Credentials = temporary(assumed.Credentials) },
	)
	_, err = roleS3.ListBuckets(ctx, &s3.ListBucketsInput{})
	mustAuthorize(t, err)
	_, err = roleS3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("role-bucket")})
	wantCode(t, err, "AccessDenied")
	if !strings.Contains(err.Error(), "arn:aws:sts::000000000000:assumed-role/reader/session") {
		t.Fatal(err)
	}

	// A bucket grant to the IAM role ARN also grants its assumed-role session,
	// even though the role has no identity permission for this bucket action.
	rootS3 := s3Client(s, "test", "us-east-1")
	_, err = rootS3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("role-policy")})
	mustAuthorize(t, err)
	_, err = rootS3.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: new("role-policy"),
		Policy: new(`{"Statement":{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::000000000000:role/apps/reader"},
 "Action":"s3:ListBucket","Resource":"arn:aws:s3:::role-policy",
 "Condition":{"StringEquals":{"aws:PrincipalTag/team":"blue"}}}}`),
	})
	mustAuthorize(t, err)
	_, err = roleS3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: new("role-policy")})
	mustAuthorize(t, err)

	// GetCallerIdentity and GetSessionToken require authentication but no permissions.
	_, err = userSTS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	mustAuthorize(t, err)
	issued, err := userSTS.GetSessionToken(ctx, &sts.GetSessionTokenInput{})
	mustAuthorize(t, err)
	userSession := s3Client(
		s,
		"ignored",
		"us-east-1",
		func(o *s3.Options) { o.Credentials = temporary(issued.Credentials) },
	)
	_, err = userSession.ListBuckets(ctx, &s3.ListBucketsInput{})
	wantCode(t, err, "AccessDenied")
	authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow",
"Action":"s3:ListAllMyBuckets",
"Resource":"*",
"Condition":{"StringEquals":{"aws:username":"alice"}}}}`)
	_, err = userSession.ListBuckets(ctx, &s3.ListBucketsInput{})
	mustAuthorize(t, err)

	// Deleting and recreating the user does not transfer the old session.
	_, err = root.DeleteUserPolicy(
		ctx,
		&iam.DeleteUserPolicyInput{UserName: new("alice"), PolicyName: new("permissions")},
	)
	mustAuthorize(t, err)
	_, err = root.DeleteAccessKey(ctx, &iam.DeleteAccessKeyInput{UserName: new("alice"), AccessKeyId: key.AccessKeyId})
	mustAuthorize(t, err)
	_, err = root.DeleteUser(ctx, &iam.DeleteUserInput{UserName: new("alice")})
	mustAuthorize(t, err)
	authorizationUser(t, s)
	authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow","Action":"*","Resource":"*"}}`)
	_, err = userSession.ListBuckets(ctx, &s3.ListBucketsInput{})
	wantCode(t, err, "AccessDenied")
	if !strings.Contains(err.Error(), "IAM identity no longer exists") {
		t.Fatal(err)
	}

	rootSession, err := stsClient(s, "test", "test").GetSessionToken(ctx, &sts.GetSessionTokenInput{})
	mustAuthorize(t, err)
	_, err = s3Client(
		s,
		"ignored",
		"us-east-1",
		func(o *s3.Options) { o.Credentials = temporary(rootSession.Credentials) },
	).ListBuckets(ctx, &s3.ListBucketsInput{})
	mustAuthorize(t, err)
}

func TestIAMAuthorizationCoreResources(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, false)
	ctx := context.Background()
	key := authorizationUser(t, s)
	id := aws.ToString(key.AccessKeyId)
	rootIAM := iamClient(s, "test", "us-east-1")
	topic, err := snsClient(s, "test").CreateTopic(ctx, &sns.CreateTopicInput{Name: new("iam-topic")})
	mustAuthorize(t, err)
	secret, err := secretsClient(
		s,
		"test",
		"us-east-1",
	).CreateSecret(ctx, &secretsmanager.CreateSecretInput{Name: new("iam-secret"), SecretString: new("data")})
	mustAuthorize(t, err)
	kmsKey, err := kmsClient(s, "test", "us-east-1").CreateKey(ctx, &kms.CreateKeyInput{})
	mustAuthorize(t, err)
	_, err = kmsClient(
		s,
		"test",
		"us-east-1",
	).CreateAlias(ctx, &kms.CreateAliasInput{AliasName: new("alias/iam-key"), TargetKeyId: kmsKey.KeyMetadata.KeyId})
	mustAuthorize(t, err)
	_, err = ssmClient(
		s,
		"test",
		"us-east-1",
	).PutParameter(ctx, &ssm.PutParameterInput{
		Name: new("/app/token"), Type: ssmtypes.ParameterTypeString, Value: new("data"),
	})
	mustAuthorize(t, err)
	_, err = lambdaClient(s, "test").CreateFunction(ctx, &lambda.CreateFunctionInput{
		FunctionName: new("iam-function"),
		Role: new(
			"arn:aws:iam::000000000000:role/lambda",
		),
		Runtime: "nodejs20.x",
		Handler: new("index.handler"),
		Code:    &lambdatypes.FunctionCode{ZipFile: zipOf(t, "index.js", "exports.handler = async event => event;")},
	})
	mustAuthorize(t, err)
	document := fmt.Sprintf(`{"Statement":[
		{"Effect":"Allow",
"Action":"sns:Publish",
"Resource":%q},
		{"Effect":"Allow",
"Action":"secretsmanager:GetSecretValue",
"Resource":%q},
		{"Effect":"Allow",
"Action":"kms:DescribeKey",
"Resource":%q},
		{"Effect":"Allow",
"Action":"ssm:GetParameter",
"Resource":"arn:aws:ssm:us-east-1:000000000000:parameter/app/token"},
		{"Effect":"Allow",
"Action":"lambda:GetFunction",
"Resource":"arn:aws:lambda:us-east-1:000000000000:function:iam-function"},
		{"Effect":"Allow",
"Action":"iam:GetUser",
"Resource":"arn:aws:iam::000000000000:user/apps/alice"}]}`,
		aws.ToString(topic.TopicArn), aws.ToString(secret.ARN), aws.ToString(kmsKey.KeyMetadata.Arn))
	authorizationPolicy(t, s, document)
	_, err = snsClient(s, id).Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: new("yes")})
	mustAuthorize(t, err)
	_, err = snsClient(s, id).DeleteTopic(ctx, &sns.DeleteTopicInput{TopicArn: topic.TopicArn})
	wantCode(t, err, "AccessDenied")
	for _, ref := range []*string{secret.Name, secret.ARN} {
		_, err = secretsClient(
			s,
			id,
			"us-east-1",
		).GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: ref})
		mustAuthorize(t, err)
	}

	for _, ref := range []*string{kmsKey.KeyMetadata.KeyId, kmsKey.KeyMetadata.Arn, new("alias/iam-key")} {
		_, err = kmsClient(s, id, "us-east-1").DescribeKey(ctx, &kms.DescribeKeyInput{KeyId: ref})
		mustAuthorize(t, err)
	}

	_, err = ssmClient(s, id, "us-east-1").GetParameter(ctx, &ssm.GetParameterInput{Name: new("/app/token:1")})
	mustAuthorize(t, err)
	_, err = lambdaClient(s, id).GetFunction(ctx, &lambda.GetFunctionInput{FunctionName: new("iam-function")})
	mustAuthorize(t, err)
	_, err = iamClient(s, id, "us-east-1").GetUser(ctx, &iam.GetUserInput{})
	mustAuthorize(t, err)
	_, err = rootIAM.TagUser(
		ctx,
		&iam.TagUserInput{UserName: new("alice"), Tags: []iamtypes.Tag{{Key: new("team"), Value: new("blue")}}},
	)
	mustAuthorize(t, err)
	authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow",
"Action":"ssm:GetParameter",
"Resource":"*",
"Condition":{"StringEquals":{"aws:PrincipalTag/team":"blue",
"aws:RequestedRegion":"us-east-1",
"aws:PrincipalAccount":"000000000000"},
"Bool":{"aws:SecureTransport":"false"},
"IpAddress":{"aws:SourceIp":"127.0.0.0/8"},
"DateGreaterThan":{"aws:CurrentTime":"2020-01-01T00:00:00Z"}}}}`)
	_, err = ssmClient(s, id, "us-east-1").GetParameter(ctx, &ssm.GetParameterInput{Name: new("/app/token")})
	mustAuthorize(t, err)
	_, err = ssmClient(s, id, "us-east-1").DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: new("/app/token")})
	wantCode(t, err, "AccessDeniedException")
}

func TestIAMAuthorizationMultiResource(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, false)
	ctx := context.Background()
	root := s3Client(s, "test", "us-east-1")
	_, err := root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("copy-bucket")})
	mustAuthorize(t, err)
	_, err = root.PutObject(
		ctx,
		&s3.PutObjectInput{Bucket: new("copy-bucket"), Key: new("source"), Body: strings.NewReader("data")},
	)
	mustAuthorize(t, err)
	key := authorizationUser(t, s)
	user := authorizationS3(s, key)
	authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow",
"Action":"s3:PutObject",
"Resource":"arn:aws:s3:::copy-bucket/destination"}}`)
	_, err = user.CopyObject(
		ctx,
		&s3.CopyObjectInput{Bucket: new("copy-bucket"), Key: new("destination"), CopySource: new("copy-bucket/source")},
	)
	wantCode(t, err, "AccessDenied")
	if !strings.Contains(err.Error(), "s3:GetObject") || !strings.Contains(err.Error(), "copy-bucket/source") {
		t.Fatal(err)
	}

	authorizationPolicy(t, s, `{"Statement":[{"Effect":"Allow",
"Action":"s3:PutObject",
"Resource":"arn:aws:s3:::copy-bucket/destination"},
{"Effect":"Allow",
"Action":"s3:GetObject",
"Resource":"arn:aws:s3:::copy-bucket/source"}]}`)
	_, err = user.CopyObject(
		ctx,
		&s3.CopyObjectInput{Bucket: new("copy-bucket"), Key: new("destination"), CopySource: new("copy-bucket/source")},
	)
	mustAuthorize(t, err)
	authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow",
"Action":"s3:DeleteObject",
"Resource":"arn:aws:s3:::copy-bucket/destination"}}`)
	_, err = user.DeleteObjects(
		ctx,
		&s3.DeleteObjectsInput{
			Bucket: new("copy-bucket"),
			Delete: &s3types.Delete{
				Objects: []s3types.ObjectIdentifier{{Key: new("destination")}, {Key: new("source")}},
			},
		},
	)
	wantCode(t, err, "AccessDenied")
	// No invocation occurred: even the allowed member still exists.
	got, err := root.GetObject(ctx, &s3.GetObjectInput{Bucket: new("copy-bucket"), Key: new("destination")})
	mustAuthorize(t, err)
	io.Copy(io.Discard, got.Body)
	got.Body.Close()
}

func TestIAMAuthorizationFallbackAndUnavailablePolicies(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, false)
	ctx := context.Background()
	key := authorizationUser(t, s)
	user := cloudwatch.New(cloudwatch.Options{
		Region:       "us-east-1",
		BaseEndpoint: new(s.URL),
		Credentials:  authorizationProvider(key),
		Retryer:      aws.NopRetryer{},
	})
	_, err := iamClient(s, "test", "us-east-1").AttachUserPolicy(ctx, &iam.AttachUserPolicyInput{
		UserName: new("alice"), PolicyArn: new("arn:aws:iam::aws:policy/AWSLambdaExecute"),
	})
	mustAuthorize(t, err)
	_, err = authorizationS3(s, key).ListBuckets(ctx, &s3.ListBucketsInput{})
	wantCode(t, err, "AccessDenied")
	if !strings.Contains(err.Error(), "AWS-managed policy document unavailable") ||
		!strings.Contains(err.Error(), "Resource '*' only") {
		t.Fatal(err)
	}

	// Signing namespace monitoring must use the IAM prefix cloudwatch.
	authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow","Action":"cloudwatch:ListMetrics","Resource":"*"}}`)
	_, err = user.ListMetrics(ctx, &cloudwatch.ListMetricsInput{})
	mustAuthorize(t, err)
	_, err = user.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: new("IAM"), MetricData: []cwtypes.MetricDatum{{MetricName: new("count"), Value: new(1.0)}},
	})
	wantCode(t, err, "AccessDeniedException")
	if !strings.Contains(err.Error(), "cloudwatch:PutMetricData") ||
		!strings.Contains(err.Error(), "Resource '*' only") {
		t.Fatal(err)
	}
}
