package timestreamquery_test

import (
	"context"
	"errors"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	iamapi "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	kmsapi "github.com/amjadjibon/tinystack/internal/aws/generated/kms"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	snsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamquery"
	writeapi "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamwrite"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/services/kms"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/services/sns"
	"github.com/amjadjibon/tinystack/internal/services/timestreamquery"
	"github.com/amjadjibon/tinystack/internal/services/timestreamwrite"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func testContext(endpoint string) context.Context {
	return request.WithMetadata(context.Background(), request.Metadata{
		Scope: request.Scope{AccountID: "123456789012", Region: "us-east-1"}, Endpoint: endpoint,
	})
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *awserror.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

type env struct {
	svc      *timestreamquery.Service
	ctx      context.Context
	topicARN string
	roleARN  string
	keyID    string
}

func fixture(t *testing.T) env {
	t.Helper()
	store := memory.New(nil, nil)
	ctx := testContext("http://127.0.0.1:4566")
	tsWrite := timestreamwrite.New(store)
	snsSvc := sns.New(store, nil, nil, nil)
	iamSvc := iam.New(store)
	s3Svc := s3.New(store, nil, nil, nil, nil, nil)
	kmsSvc := kms.New(store)

	if _, err := tsWrite.CreateDatabase(ctx, &writeapi.CreateDatabaseRequest{DatabaseName: new("db1")}); err != nil {
		t.Fatal(err)
	}

	if _, err := tsWrite.CreateTable(ctx, &writeapi.CreateTableRequest{
		DatabaseName: new("db1"),
		TableName:    new("tab1"),
	}); err != nil {
		t.Fatal(err)
	}

	topic, err := snsSvc.CreateTopic(ctx, &snsapi.CreateTopicInput{Name: new("query-alerts")})
	if err != nil {
		t.Fatal(err)
	}

	role, err := iamSvc.CreateRole(ctx, &iamapi.CreateRoleRequest{
		RoleName:                 new("query-role"),
		AssumeRolePolicyDocument: new(`{"Version":"2012-10-17","Statement":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = s3Svc.CreateBucket(ctx, &s3api.CreateBucketRequest{Bucket: new("error-reports")}); err != nil {
		t.Fatal(err)
	}

	keyOut, err := kmsSvc.CreateKey(ctx, &kmsapi.CreateKeyRequest{})
	if err != nil {
		t.Fatal(err)
	}

	return env{
		svc:      timestreamquery.New(store, tsWrite, snsSvc, iamSvc, s3Svc, kmsSvc),
		ctx:      ctx,
		topicARN: *topic.TopicArn,
		roleARN:  *role.Role.Arn,
		keyID:    *keyOut.KeyMetadata.KeyId,
	}
}

func TestScheduledQueryLifecycleAndAccountSettings(t *testing.T) {
	e := fixture(t)
	endpoints, err := e.svc.DescribeEndpoints(e.ctx, &api.DescribeEndpointsRequest{})
	if err != nil || len(endpoints.Endpoints) != 1 || *endpoints.Endpoints[0].Address != "http://127.0.0.1:4566" {
		t.Fatalf("unexpected endpoints: %+v %v", endpoints, err)
	}

	query := "SELECT BIN(time, 1h) AS binned_time, AVG(cpu) AS avg_cpu FROM db1.tab1 GROUP BY BIN(time, 1h)"
	req := &api.CreateScheduledQueryRequest{
		Name:        new("hourly-rollup"),
		QueryString: new(query),
		ScheduleConfiguration: &api.ScheduleConfiguration{
			ScheduleExpression: new("rate(1 hour)"),
		},
		NotificationConfiguration: &api.NotificationConfiguration{
			SnsConfiguration: &api.SnsConfiguration{TopicArn: new(e.topicARN)},
		},
		TargetConfiguration: &api.TargetConfiguration{
			TimestreamConfiguration: &api.TimestreamConfiguration{
				DatabaseName:      new("db1"),
				TableName:         new("tab1"),
				TimeColumn:        new("binned_time"),
				DimensionMappings: []api.DimensionMapping{},
				MultiMeasureMappings: &api.MultiMeasureMappings{
					MultiMeasureAttributeMappings: []api.MultiMeasureAttributeMapping{
						{SourceColumn: new("avg_cpu"), MeasureValueType: new("DOUBLE")},
					},
				},
			},
		},
		ScheduledQueryExecutionRoleArn: new(e.roleARN),
		ErrorReportConfiguration: &api.ErrorReportConfiguration{
			S3Configuration: &api.S3Configuration{BucketName: new("error-reports")},
		},
		KmsKeyId:    new(e.keyID),
		ClientToken: new("12345678901234567890123456789012"),
		Tags:        []api.Tag{{Key: new("env"), Value: new("test")}},
	}

	created, err := e.svc.CreateScheduledQuery(e.ctx, req)
	if err != nil || created.Arn == nil {
		t.Fatalf("CreateScheduledQuery failed: %v", err)
	}

	// Idempotent retry with same ClientToken and parameters returns same ARN.
	dup, err := e.svc.CreateScheduledQuery(e.ctx, req)
	if err != nil || *dup.Arn != *created.Arn {
		t.Fatalf("idempotent CreateScheduledQuery mismatch: %+v %v", dup, err)
	}

	// Same ClientToken with different query fails with ConflictException.
	conflictReq := *req
	conflictReq.QueryString = new("SELECT 1")
	_, err = e.svc.CreateScheduledQuery(e.ctx, &conflictReq)
	wantCode(t, err, "ConflictException")

	desc, err := e.svc.DescribeScheduledQuery(e.ctx, &api.DescribeScheduledQueryRequest{
		ScheduledQueryArn: created.Arn,
	})
	if err != nil || desc.ScheduledQuery.NextInvocationTime == nil || *desc.ScheduledQuery.State != "ENABLED" {
		t.Fatalf("DescribeScheduledQuery unexpected: %+v %v", desc, err)
	}

	if _, err = e.svc.UpdateScheduledQuery(e.ctx, &api.UpdateScheduledQueryRequest{
		ScheduledQueryArn: created.Arn,
		State:             new("DISABLED"),
	}); err != nil {
		t.Fatal(err)
	}

	settings, err := e.svc.UpdateAccountSettings(e.ctx, &api.UpdateAccountSettingsRequest{
		MaxQueryTCU:       new(int64(32)),
		QueryPricingModel: new("COMPUTE_UNITS"),
		QueryCompute: &api.QueryComputeRequest{
			ComputeMode: new("PROVISIONED"),
			ProvisionedCapacity: &api.ProvisionedCapacityRequest{
				TargetQueryTCU: new(int64(16)),
				NotificationConfiguration: &api.AccountSettingsNotificationConfiguration{
					RoleArn:          new(e.roleARN),
					SnsConfiguration: &api.SnsConfiguration{TopicArn: new(e.topicARN)},
				},
			},
		},
	})
	if err != nil || *settings.MaxQueryTCU != 32 || *settings.QueryCompute.ProvisionedCapacity.ActiveQueryTCU != 16 {
		t.Fatalf("UpdateAccountSettings unexpected: %+v %v", settings, err)
	}
}
