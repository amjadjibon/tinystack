package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroups"
	rgtypes "github.com/aws/aws-sdk-go-v2/service/resourcegroups/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	sdk "github.com/aws/aws-sdk-go-v2/service/timestreamquery"
	"github.com/aws/aws-sdk-go-v2/service/timestreamquery/types"
	tw "github.com/aws/aws-sdk-go-v2/service/timestreamwrite"
)

func timestreamQueryClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

type timestreamQueryEnv struct {
	query     *sdk.Client
	write     *tw.Client
	topicARN  string
	roleARN   string
	bucket    string
	kmsKeyARN string
	dbName    string
	tableName string
}

func timestreamQuerySetup(t *testing.T, s *httptest.Server) *timestreamQueryEnv {
	t.Helper()
	ctx := context.Background()

	topic, err := snsClient(s, "test").CreateTopic(ctx, &sns.CreateTopicInput{
		Name: new("tsq-notifications"),
	})
	if err != nil {
		t.Fatal(err)
	}

	role, err := iamClient(s, "test", "us-east-1").CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 new("tsq-exec-role"),
		AssumeRolePolicyDocument: new(`{"Version":"2012-10-17","Statement":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	bucket := "tsq-error-reports"
	if _, err = s3Client(s, "test", "us-east-1").CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: new(bucket),
	}); err != nil {
		t.Fatal(err)
	}

	key, err := kmsClient(s, "test", "us-east-1").CreateKey(ctx, &kms.CreateKeyInput{})
	if err != nil {
		t.Fatal(err)
	}

	write := timestreamWriteClient(s, "test", "us-east-1")
	dbName := "tsq-metrics"
	tableName := "tsq-rollup"
	if _, err = write.CreateDatabase(ctx, &tw.CreateDatabaseInput{DatabaseName: new(dbName)}); err != nil {
		t.Fatal(err)
	}

	if _, err = write.CreateTable(ctx, &tw.CreateTableInput{
		DatabaseName: new(dbName),
		TableName:    new(tableName),
	}); err != nil {
		t.Fatal(err)
	}

	return &timestreamQueryEnv{
		query:     timestreamQueryClient(s, "test", "us-east-1"),
		write:     write,
		topicARN:  *topic.TopicArn,
		roleARN:   *role.Role.Arn,
		bucket:    bucket,
		kmsKeyARN: *key.KeyMetadata.Arn,
		dbName:    dbName,
		tableName: tableName,
	}
}

func tsqToken(seed string) string {
	if len(seed) >= 32 {
		return seed[:32]
	}

	return seed + strings.Repeat("0", 32-len(seed))
}

func (e *timestreamQueryEnv) createInput(name, token string) *sdk.CreateScheduledQueryInput {
	return &sdk.CreateScheduledQueryInput{
		Name:        new(name),
		ClientToken: new(tsqToken(token)),
		QueryString: new("SELECT BIN(time, 1m) AS binned_timestamp, AVG(cpu) AS avg_cpu FROM tsq"),
		ScheduleConfiguration: &types.ScheduleConfiguration{
			ScheduleExpression: new("rate(1 hour)"),
		},
		NotificationConfiguration: &types.NotificationConfiguration{
			SnsConfiguration: &types.SnsConfiguration{
				TopicArn: new(e.topicARN),
			},
		},
		ErrorReportConfiguration: &types.ErrorReportConfiguration{
			S3Configuration: &types.S3Configuration{
				BucketName:       new(e.bucket),
				ObjectKeyPrefix:  new("errors/"),
				EncryptionOption: types.S3EncryptionOptionSseS3,
			},
		},
		ScheduledQueryExecutionRoleArn: new(e.roleARN),
		KmsKeyId:                       new(e.kmsKeyARN),
		TargetConfiguration: &types.TargetConfiguration{
			TimestreamConfiguration: &types.TimestreamConfiguration{
				DatabaseName: new(e.dbName),
				TableName:    new(e.tableName),
				TimeColumn:   new("binned_timestamp"),
				DimensionMappings: []types.DimensionMapping{
					{
						Name:               new("host"),
						DimensionValueType: types.DimensionValueTypeVarchar,
					},
				},
				MultiMeasureMappings: &types.MultiMeasureMappings{
					MultiMeasureAttributeMappings: []types.MultiMeasureAttributeMapping{
						{
							SourceColumn:     new("avg_cpu"),
							MeasureValueType: types.ScalarMeasureValueTypeDouble,
						},
					},
				},
			},
		},
		Tags: []types.Tag{
			{Key: new("env"), Value: new("dev")},
			{Key: new("team"), Value: new("core")},
		},
	}
}

func TestTimestreamQuerySDKLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := server(t)
	env := timestreamQuerySetup(t, s)
	c := env.query

	endpoints, err := c.DescribeEndpoints(ctx, &sdk.DescribeEndpointsInput{})
	if err != nil || len(endpoints.Endpoints) != 1 || aws.ToString(endpoints.Endpoints[0].Address) == "" {
		t.Fatalf("DescribeEndpoints: %+v %v", endpoints, err)
	}

	// Account settings defaults and updates.
	settings, err := c.DescribeAccountSettings(ctx, &sdk.DescribeAccountSettingsInput{})
	if err != nil || aws.ToInt32(settings.MaxQueryTCU) != 1000 ||
		settings.QueryPricingModel != types.QueryPricingModelBytesScanned ||
		settings.QueryCompute == nil || settings.QueryCompute.ComputeMode != types.ComputeModeOnDemand {
		t.Fatalf("default account settings: %+v %v", settings, err)
	}

	updatedSettings, err := c.UpdateAccountSettings(ctx, &sdk.UpdateAccountSettingsInput{
		MaxQueryTCU:       new(int32(32)),
		QueryPricingModel: types.QueryPricingModelComputeUnits,
		QueryCompute: &types.QueryComputeRequest{
			ComputeMode: types.ComputeModeProvisioned,
			ProvisionedCapacity: &types.ProvisionedCapacityRequest{
				TargetQueryTCU: new(int32(12)),
				NotificationConfiguration: &types.AccountSettingsNotificationConfiguration{
					RoleArn: new(env.roleARN),
					SnsConfiguration: &types.SnsConfiguration{
						TopicArn: new(env.topicARN),
					},
				},
			},
		},
	})
	if err != nil || aws.ToInt32(updatedSettings.MaxQueryTCU) != 32 ||
		updatedSettings.QueryPricingModel != types.QueryPricingModelComputeUnits ||
		updatedSettings.QueryCompute == nil ||
		updatedSettings.QueryCompute.ComputeMode != types.ComputeModeProvisioned ||
		updatedSettings.QueryCompute.ProvisionedCapacity == nil ||
		aws.ToInt32(updatedSettings.QueryCompute.ProvisionedCapacity.ActiveQueryTCU) != 12 {
		t.Fatalf("updated settings: %+v %v", updatedSettings, err)
	}

	_, err = c.UpdateAccountSettings(ctx, &sdk.UpdateAccountSettingsInput{
		MaxQueryTCU: new(int32(5)),
	})
	wantTimestreamError(t, err, "ValidationException")

	// Cross-service reference errors on CreateScheduledQuery.
	badTopic := env.createInput("bad-topic", "tok-bad-topic")
	badTopic.NotificationConfiguration.SnsConfiguration.TopicArn = new("arn:aws:sns:us-east-1:000000000000:missing")
	_, err = c.CreateScheduledQuery(ctx, badTopic)
	wantTimestreamError(t, err, "ValidationException")

	badRole := env.createInput("bad-role", "tok-bad-role")
	badRole.ScheduledQueryExecutionRoleArn = new("arn:aws:iam::000000000000:role/missing")
	_, err = c.CreateScheduledQuery(ctx, badRole)
	wantTimestreamError(t, err, "ValidationException")

	badBucket := env.createInput("bad-bucket", "tok-bad-bucket")
	badBucket.ErrorReportConfiguration.S3Configuration.BucketName = new("missing-bucket")
	_, err = c.CreateScheduledQuery(ctx, badBucket)
	wantTimestreamError(t, err, "ValidationException")

	badTable := env.createInput("bad-table", "tok-bad-table")
	badTable.TargetConfiguration.TimestreamConfiguration.TableName = new("missing-table")
	_, err = c.CreateScheduledQuery(ctx, badTable)
	wantTimestreamError(t, err, "ValidationException")

	badKMS := env.createInput("bad-kms", "tok-bad-kms")
	badKMS.KmsKeyId = new("arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000000")
	_, err = c.CreateScheduledQuery(ctx, badKMS)
	wantTimestreamError(t, err, "ValidationException")

	badSchedule := env.createInput("bad-schedule", "tok-bad-schedule")
	badSchedule.ScheduleConfiguration.ScheduleExpression = new("not-a-schedule")
	_, err = c.CreateScheduledQuery(ctx, badSchedule)
	wantTimestreamError(t, err, "ValidationException")

	// Create scheduled query and verify idempotency and conflict detection.
	in := env.createInput("hourly-cpu", "token-hourly-cpu")
	created, err := c.CreateScheduledQuery(ctx, in)
	if err != nil || aws.ToString(created.Arn) == "" {
		t.Fatalf("CreateScheduledQuery: %+v %v", created, err)
	}

	replayed, err := c.CreateScheduledQuery(ctx, in)
	if err != nil || aws.ToString(replayed.Arn) != aws.ToString(created.Arn) {
		t.Fatalf("idempotent replay: %+v %v", replayed, err)
	}

	changedToken := env.createInput("hourly-cpu-other", "token-hourly-cpu")
	_, err = c.CreateScheduledQuery(ctx, changedToken)
	wantTimestreamError(t, err, "ConflictException")

	dupName := env.createInput("hourly-cpu", "token-different")
	_, err = c.CreateScheduledQuery(ctx, dupName)
	wantTimestreamError(t, err, "ConflictException")

	// DescribeScheduledQuery verifies record fields and NextInvocationTime.
	desc, err := c.DescribeScheduledQuery(ctx, &sdk.DescribeScheduledQueryInput{
		ScheduledQueryArn: created.Arn,
	})
	if err != nil || desc.ScheduledQuery == nil {
		t.Fatalf("DescribeScheduledQuery: %+v %v", desc, err)
	}

	sq := desc.ScheduledQuery
	if aws.ToString(sq.Name) != "hourly-cpu" ||
		sq.State != types.ScheduledQueryStateEnabled ||
		sq.CreationTime == nil ||
		sq.NextInvocationTime == nil ||
		!sq.NextInvocationTime.After(time.Now().Add(-time.Minute)) ||
		aws.ToString(sq.KmsKeyId) != env.kmsKeyARN ||
		aws.ToString(sq.ScheduledQueryExecutionRoleArn) != env.roleARN ||
		sq.TargetConfiguration == nil ||
		sq.TargetConfiguration.TimestreamConfiguration == nil ||
		aws.ToString(sq.TargetConfiguration.TimestreamConfiguration.DatabaseName) != env.dbName {
		t.Fatalf("unexpected ScheduledQuery description: %+v", sq)
	}

	// Second scheduled query for pagination testing.
	secondIn := env.createInput("daily-cpu", "token-daily-cpu")
	secondIn.ScheduleConfiguration.ScheduleExpression = new("cron(0 12 * * ? *)")
	second, err := c.CreateScheduledQuery(ctx, secondIn)
	if err != nil {
		t.Fatal(err)
	}

	var listedARNs []string
	var nextToken *string
	for {
		page, pageErr := c.ListScheduledQueries(ctx, &sdk.ListScheduledQueriesInput{
			MaxResults: new(int32(1)),
			NextToken:  nextToken,
		})
		if pageErr != nil {
			t.Fatal(pageErr)
		}

		for _, item := range page.ScheduledQueries {
			listedARNs = append(listedARNs, aws.ToString(item.Arn))
		}

		if nextToken = page.NextToken; nextToken == nil {
			break
		}
	}

	wantARNs := []string{aws.ToString(second.Arn), aws.ToString(created.Arn)}
	slices.Sort(wantARNs)
	if !slices.Equal(listedARNs, wantARNs) {
		t.Fatalf("ListScheduledQueries paged %v, want %v", listedARNs, wantARNs)
	}

	_, err = c.ListScheduledQueries(ctx, &sdk.ListScheduledQueriesInput{NextToken: new("invalid-token")})
	wantTimestreamError(t, err, "ValidationException")

	// UpdateScheduledQuery state transitions.
	if _, err = c.UpdateScheduledQuery(ctx, &sdk.UpdateScheduledQueryInput{
		ScheduledQueryArn: created.Arn,
		State:             types.ScheduledQueryStateDisabled,
	}); err != nil {
		t.Fatal(err)
	}

	disabledDesc, err := c.DescribeScheduledQuery(ctx, &sdk.DescribeScheduledQueryInput{
		ScheduledQueryArn: created.Arn,
	})
	if err != nil || disabledDesc.ScheduledQuery.State != types.ScheduledQueryStateDisabled ||
		disabledDesc.ScheduledQuery.NextInvocationTime != nil {
		t.Fatalf("expected DISABLED state without a next invocation, got %+v %v", disabledDesc, err)
	}

	if _, err = c.UpdateScheduledQuery(ctx, &sdk.UpdateScheduledQueryInput{
		ScheduledQueryArn: created.Arn,
		State:             types.ScheduledQueryStateEnabled,
	}); err != nil {
		t.Fatal(err)
	}

	enabledDesc, err := c.DescribeScheduledQuery(ctx, &sdk.DescribeScheduledQueryInput{
		ScheduledQueryArn: created.Arn,
	})
	if err != nil || enabledDesc.ScheduledQuery.NextInvocationTime == nil ||
		!enabledDesc.ScheduledQuery.NextInvocationTime.After(time.Now()) {
		t.Fatalf("expected a future next invocation after re-enabling, got %+v %v", enabledDesc, err)
	}

	if _, err = c.UpdateScheduledQuery(ctx, &sdk.UpdateScheduledQueryInput{
		ScheduledQueryArn: created.Arn,
		State:             types.ScheduledQueryStateDisabled,
	}); err != nil {
		t.Fatal(err)
	}

	// Tagging via service API and Resource Groups Tagging API.
	if _, err = c.TagResource(ctx, &sdk.TagResourceInput{
		ResourceARN: created.Arn,
		Tags:        []types.Tag{{Key: new("cost-center"), Value: new("analytics")}},
	}); err != nil {
		t.Fatal(err)
	}

	var tagKeys []string
	var tagToken *string
	for {
		tagPage, tagErr := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
			ResourceARN: created.Arn,
			MaxResults:  new(int32(2)),
			NextToken:   tagToken,
		})
		if tagErr != nil {
			t.Fatal(tagErr)
		}

		for _, tag := range tagPage.Tags {
			tagKeys = append(tagKeys, aws.ToString(tag.Key))
		}

		if tagToken = tagPage.NextToken; tagToken == nil {
			break
		}
	}

	if !slices.Equal(tagKeys, []string{"cost-center", "env", "team"}) {
		t.Fatalf("paged tag keys: %v", tagKeys)
	}

	if _, err = c.UntagResource(ctx, &sdk.UntagResourceInput{
		ResourceARN: created.Arn,
		TagKeys:     []string{"cost-center"},
	}); err != nil {
		t.Fatal(err)
	}

	tagging := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	rgTagged, err := tagging.TagResources(ctx, &rgta.TagResourcesInput{
		ResourceARNList: []string{aws.ToString(created.Arn)},
		Tags:            map[string]string{"rgta": "ok"},
	})
	if err != nil || len(rgTagged.FailedResourcesMap) != 0 {
		t.Fatalf("RGTA TagResources: %+v %v", rgTagged, err)
	}

	rgFound, err := tagging.GetResources(ctx, &rgta.GetResourcesInput{
		ResourceTypeFilters: []string{"timestream:scheduled-query"},
	})
	if err != nil || len(rgFound.ResourceTagMappingList) != 2 {
		t.Fatalf("RGTA GetResources: %+v %v", rgFound, err)
	}

	// Resource Groups selection for AWS::Timestream::ScheduledQuery.
	rg := resourcegroups.New(resourcegroups.Options{
		Region:       "us-east-1",
		BaseEndpoint: new(s.URL),
		Credentials:  creds("test"),
	})
	rgQuery := `{"ResourceTypeFilters":["AWS::Timestream::ScheduledQuery"],"TagFilters":[{"Key":"rgta","Values":["ok"]}]}`
	searched, err := rg.SearchResources(ctx, &resourcegroups.SearchResourcesInput{
		ResourceQuery: &rgtypes.ResourceQuery{
			Type:  rgtypes.QueryTypeTagFilters10,
			Query: new(rgQuery),
		},
	})
	if err != nil || len(searched.ResourceIdentifiers) != 1 ||
		aws.ToString(searched.ResourceIdentifiers[0].ResourceArn) != aws.ToString(created.Arn) {
		t.Fatalf("ResourceGroups SearchResources: %+v %v", searched, err)
	}

	// Inventory reports RecordsOnly=false and ExecutionReady=true.
	var inv struct {
		Resources []struct {
			Details map[string]any `json:"details"`
			Service string         `json:"service"`
			Type    string         `json:"type"`
			Name    string         `json:"name"`
			ARN     string         `json:"arn"`
		} `json:"resources"`
	}

	if status := internal(t, s, http.MethodGet, "/_tinystack/resources", &inv); status != http.StatusOK {
		t.Fatalf("inventory status %d", status)
	}

	foundInventory := false
	for _, r := range inv.Resources {
		if r.Service == "timestreamquery" && r.Type == "scheduled-query" && r.Name == "hourly-cpu" {
			foundInventory = true
			if r.Details["RecordsOnly"] != false || r.Details["ExecutionReady"] != true {
				t.Fatalf("unexpected inventory item: %+v", r)
			}
		}
	}

	if !foundInventory {
		t.Fatalf("scheduled-query hourly-cpu not found in inventory")
	}

	// Account and region isolation.
	for _, foreign := range []*sdk.Client{
		timestreamQueryClient(s, "team-b", "us-east-1"),
		timestreamQueryClient(s, "test", "eu-west-1"),
	} {
		_, err = foreign.DescribeScheduledQuery(ctx, &sdk.DescribeScheduledQueryInput{
			ScheduledQueryArn: created.Arn,
		})
		wantTimestreamError(t, err, "ResourceNotFoundException")
	}

	// Delete scheduled queries.
	for _, arn := range []*string{created.Arn, second.Arn} {
		if _, err = c.DeleteScheduledQuery(ctx, &sdk.DeleteScheduledQueryInput{
			ScheduledQueryArn: arn,
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.DescribeScheduledQuery(ctx, &sdk.DescribeScheduledQueryInput{
		ScheduledQueryArn: created.Arn,
	})
	wantTimestreamError(t, err, "ResourceNotFoundException")
}

func TestTimestreamQueryCloudFormation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := server(t)
	env := timestreamQuerySetup(t, s)
	c := cfn.New(cfn.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})

	template := func(name, queryString, team string) *string {
		props := map[string]any{
			"QueryString":                    queryString,
			"ScheduledQueryExecutionRoleArn": env.roleARN,
			"KmsKeyId":                       env.kmsKeyARN,
			"ScheduleConfiguration": map[string]any{
				"ScheduleExpression": "rate(1 hour)",
			},
			"NotificationConfiguration": map[string]any{
				"SnsConfiguration": map[string]any{
					"TopicArn": env.topicARN,
				},
			},
			"ErrorReportConfiguration": map[string]any{
				"S3Configuration": map[string]any{
					"BucketName":       env.bucket,
					"ObjectKeyPrefix":  "cfn-errors/",
					"EncryptionOption": "SSE_S3",
				},
			},
			"TargetConfiguration": map[string]any{
				"TimestreamConfiguration": map[string]any{
					"DatabaseName": env.dbName,
					"TableName":    env.tableName,
					"TimeColumn":   "binned_timestamp",
					"DimensionMappings": []map[string]any{
						{
							"Name":               "host",
							"DimensionValueType": "VARCHAR",
						},
					},
					"MultiMeasureMappings": map[string]any{
						"MultiMeasureAttributeMappings": []map[string]any{
							{
								"SourceColumn":     "avg_cpu",
								"MeasureValueType": "DOUBLE",
							},
						},
					},
				},
			},
			"Tags": []map[string]string{
				{"Key": "team", "Value": team},
			},
		}

		if name != "" {
			props["ScheduledQueryName"] = name
		}

		getAtt := func(attr string) map[string]any {
			return map[string]any{"Value": map[string]any{"Fn::GetAtt": []string{"ScheduledQuery", attr}}}
		}

		doc := map[string]any{
			"Resources": map[string]any{
				"ScheduledQuery": map[string]any{
					"Type":       "AWS::Timestream::ScheduledQuery",
					"Properties": props,
				},
			},
			"Outputs": map[string]any{
				"Ref":     map[string]any{"Value": map[string]string{"Ref": "ScheduledQuery"}},
				"Arn":     getAtt("Arn"),
				"SQName":  getAtt("SQName"),
				"SQQuery": getAtt("SQQueryString"),
				"SQRole":  getAtt("SQScheduledQueryExecutionRoleArn"),
			},
		}

		data, _ := json.Marshal(doc)
		return new(string(data))
	}

	if _, err := c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName:    new("tsq-stack"),
		TemplateBody: template("cfn-query", "SELECT 1", "core"),
	}); err != nil {
		t.Fatal(err)
	}

	stack := settled(ctx, t, c, "tsq-stack")
	out := outputsOf(&stack)
	if string(stack.StackStatus) != "CREATE_COMPLETE" || out["Ref"] == "" ||
		out["Arn"] != out["Ref"] || out["SQName"] != "cfn-query" || out["SQRole"] != env.roleARN {
		t.Fatalf("CreateStack: %+v %v", stack, out)
	}

	// Tag change updates in-place without replacement.
	if _, err := c.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName:    new("tsq-stack"),
		TemplateBody: template("cfn-query", "SELECT 1", "platform"),
	}); err != nil {
		t.Fatal(err)
	}

	stack = settled(ctx, t, c, "tsq-stack")
	updatedOut := outputsOf(&stack)
	if string(stack.StackStatus) != "UPDATE_COMPLETE" || updatedOut["Arn"] != out["Arn"] {
		t.Fatalf("in-place UpdateStack: %+v %v", stack, updatedOut)
	}

	tags, err := env.query.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
		ResourceARN: new(updatedOut["Arn"]),
	})
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Value) != "platform" {
		t.Fatalf("tags after UpdateStack: %+v %v", tags, err)
	}

	// Changing an immutable property (and omitting ScheduledQueryName to exercise generated name) replaces the resource.
	if _, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName:    new("tsq-stack"),
		TemplateBody: template("", "SELECT 2", "platform"),
	}); err != nil {
		t.Fatal(err)
	}

	stack = settled(ctx, t, c, "tsq-stack")
	replacedOut := outputsOf(&stack)
	if string(stack.StackStatus) != "UPDATE_COMPLETE" ||
		replacedOut["Arn"] == updatedOut["Arn"] ||
		replacedOut["SQName"] == "" ||
		replacedOut["SQName"] == "cfn-query" ||
		replacedOut["SQQuery"] != "SELECT 2" {
		t.Fatalf("replacement UpdateStack: %+v %v", stack, replacedOut)
	}

	_, err = env.query.DescribeScheduledQuery(ctx, &sdk.DescribeScheduledQueryInput{
		ScheduledQueryArn: new(updatedOut["Arn"]),
	})
	wantTimestreamError(t, err, "ResourceNotFoundException")

	if _, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("tsq-stack")}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "tsq-stack deletion", func() bool {
		_, e := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: new("tsq-stack")})
		return e != nil
	})

	_, err = env.query.DescribeScheduledQuery(ctx, &sdk.DescribeScheduledQueryInput{
		ScheduledQueryArn: new(replacedOut["Arn"]),
	})
	wantTimestreamError(t, err, "ResourceNotFoundException")
}

func TestTimestreamQuerySQLiteRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	t.Cleanup(func() { stop() })

	env := timestreamQuerySetup(t, s)
	if _, err := env.query.UpdateAccountSettings(ctx, &sdk.UpdateAccountSettingsInput{
		MaxQueryTCU:       new(int32(128)),
		QueryPricingModel: types.QueryPricingModelComputeUnits,
	}); err != nil {
		t.Fatal(err)
	}

	created, err := env.query.CreateScheduledQuery(ctx, env.createInput("persist-query", "persist-token"))
	if err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	c := timestreamQueryClient(s, "test", "us-east-1")

	settings, err := c.DescribeAccountSettings(ctx, &sdk.DescribeAccountSettingsInput{})
	if err != nil || aws.ToInt32(settings.MaxQueryTCU) != 128 ||
		settings.QueryPricingModel != types.QueryPricingModelComputeUnits {
		t.Fatalf("persisted account settings: %+v %v", settings, err)
	}

	desc, err := c.DescribeScheduledQuery(ctx, &sdk.DescribeScheduledQueryInput{
		ScheduledQueryArn: created.Arn,
	})
	if err != nil || aws.ToString(desc.ScheduledQuery.Name) != "persist-query" {
		t.Fatalf("persisted scheduled query: %+v %v", desc, err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
		ResourceARN: created.Arn,
	})
	if err != nil || len(tags.Tags) != 2 {
		t.Fatalf("persisted tags: %+v %v", tags, err)
	}
}

func TestTimestreamQueryTokenReplay(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := timestreamQueryClient(s, "test", "us-east-1")
	in := &sdk.QueryInput{QueryString: new("SELECT 1"), ClientToken: new(strings.Repeat("a", 32))}
	first, err := c.Query(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := c.Query(ctx, in)
	if err != nil || *first.QueryId != *replay.QueryId {
		t.Fatalf("query replay: %+v %v", replay, err)
	}
	in.QueryString = new("SELECT 2")
	_, err = c.Query(ctx, in)
	wantTimestreamError(t, err, "ValidationException")
	env := timestreamQuerySetup(t, s)
	created, err := c.CreateScheduledQuery(ctx, env.createInput("explicit-once", "explicit-create"))
	if err != nil {
		t.Fatal(err)
	}
	invocation := &sdk.ExecuteScheduledQueryInput{
		ScheduledQueryArn: created.Arn,
		InvocationTime:    new(time.Now().UTC()),
		ClientToken:       new(strings.Repeat("b", 32)),
	}
	if _, err = c.ExecuteScheduledQuery(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ExecuteScheduledQuery(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	changed := *invocation
	changed.InvocationTime = new(invocation.InvocationTime.Add(time.Minute))
	_, err = c.ExecuteScheduledQuery(ctx, &changed)
	wantTimestreamError(t, err, "ValidationException")
}
