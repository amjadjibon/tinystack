package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/configservice"
	"github.com/aws/aws-sdk-go-v2/service/configservice/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func configClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func configRule(name string) *types.ConfigRule {
	return &types.ConfigRule{
		ConfigRuleName: new(name),
		Description:    new("metadata only"),
		Source:         &types.Source{Owner: types.OwnerAws, SourceIdentifier: new("REQUIRED_TAGS")},
		InputParameters: new(
			`{"tag1Key":"CostCenter"}`,
		),
		Scope: &types.Scope{ComplianceResourceTypes: []string{"AWS::S3::Bucket"}},
	}
}

func configRecorder(name string) *types.ConfigurationRecorder {
	return &types.ConfigurationRecorder{
		Name:    new(name),
		RoleARN: new("arn:aws:iam::000000000000:role/config-role"),
		RecordingGroup: &types.RecordingGroup{
			ResourceTypes: []types.ResourceType{types.ResourceType("AWS::S3::Bucket")},
		},
	}
}

func configChannel(name string) *types.DeliveryChannel {
	return &types.DeliveryChannel{
		Name:         new(name),
		S3BucketName: new("config-destination"),
		S3KeyPrefix:  new("config/persisted"),
		SnsTopicARN:  new("arn:aws:sns:us-east-1:000000000000:config-notifications"),
		ConfigSnapshotDeliveryProperties: &types.ConfigSnapshotDeliveryProperties{
			DeliveryFrequency: types.MaximumExecutionFrequencySixHours,
		},
	}
}

func putConfigFixture(t *testing.T, s *httptest.Server, c *sdk.Client) {
	t.Helper()
	ctx := context.Background()
	if _, err := s3Client(
		s,
		"test",
		"us-east-1",
	).CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("config-destination")}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PutConfigurationRecorder(ctx, &sdk.PutConfigurationRecorderInput{
		ConfigurationRecorder: configRecorder("primary"),
		Tags:                  []types.Tag{{Key: new("owner"), Value: new("platform")}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PutDeliveryChannel(
		ctx,
		&sdk.PutDeliveryChannelInput{DeliveryChannel: configChannel("primary")},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PutConfigRule(ctx, &sdk.PutConfigRuleInput{
		ConfigRule: configRule("required-tags"),
		Tags:       []types.Tag{{Key: new("env"), Value: new("dev")}, {Key: new("owner"), Value: new("platform")}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestConfigSDKWorkflow(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := configClient(s, "test", "us-east-1")
	_, err := c.StartConfigurationRecorder(
		ctx,
		&sdk.StartConfigurationRecorderInput{ConfigurationRecorderName: new("primary")},
	)
	code(t, err, "NoSuchConfigurationRecorderException")
	_, err = c.PutConfigurationRecorder(
		ctx,
		&sdk.PutConfigurationRecorderInput{ConfigurationRecorder: configRecorder("primary")},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.StartConfigurationRecorder(
		ctx,
		&sdk.StartConfigurationRecorderInput{ConfigurationRecorderName: new("primary")},
	)
	code(t, err, "NoAvailableDeliveryChannelException")
	putConfigFixture(t, s, c)
	rule, err := c.DescribeConfigRules(ctx, &sdk.DescribeConfigRulesInput{ConfigRuleNames: []string{"required-tags"}})
	if err != nil || len(rule.ConfigRules) != 1 {
		t.Fatalf("rule: %+v %v", rule, err)
	}
	original := rule.ConfigRules[0]
	update := configRule("required-tags")
	update.Description = new("updated")
	update.InputParameters = new(`{"tag1Key":"Owner"}`)
	_, err = c.PutConfigRule(
		ctx,
		&sdk.PutConfigRuleInput{ConfigRule: update, Tags: []types.Tag{{Key: new("env"), Value: new("ignored")}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	rule, err = c.DescribeConfigRules(ctx, &sdk.DescribeConfigRulesInput{})
	if err != nil || len(rule.ConfigRules) != 1 ||
		aws.ToString(rule.ConfigRules[0].ConfigRuleId) != aws.ToString(original.ConfigRuleId) ||
		aws.ToString(rule.ConfigRules[0].ConfigRuleArn) != aws.ToString(original.ConfigRuleArn) ||
		aws.ToString(rule.ConfigRules[0].Description) != "updated" {
		t.Fatalf("update/identity: %+v %v", rule, err)
	}
	// Update by ID alone also retains the rule's name and identity.
	update.ConfigRuleName = nil
	update.ConfigRuleId = original.ConfigRuleId
	if _, err = c.PutConfigRule(ctx, &sdk.PutConfigRuleInput{ConfigRule: update}); err != nil {
		t.Fatal(err)
	}
	tags, err := c.ListTagsForResource(
		ctx,
		&sdk.ListTagsForResourceInput{ResourceArn: original.ConfigRuleArn, Limit: 1},
	)
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Value) != "dev" || tags.NextToken == nil {
		t.Fatalf("tags page: %+v %v", tags, err)
	}
	tags, err = c.ListTagsForResource(
		ctx,
		&sdk.ListTagsForResourceInput{ResourceArn: original.ConfigRuleArn, Limit: 1, NextToken: tags.NextToken},
	)
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Key) != "owner" || tags.NextToken != nil {
		t.Fatalf("tags second: %+v %v", tags, err)
	}
	_, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{
			ResourceArn: original.ConfigRuleArn,
			Tags:        []types.Tag{{Key: new("env"), Value: new("prod")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.UntagResource(
		ctx,
		&sdk.UntagResourceInput{ResourceArn: original.ConfigRuleArn, TagKeys: []string{"owner"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	// Shared inventory/tagging routes reach the owning Config service.
	shared := taggingClient(s, "us-east-1")
	tagged, err := shared.TagResources(
		ctx,
		&resourcegroupstaggingapi.TagResourcesInput{
			ResourceARNList: []string{aws.ToString(original.ConfigRuleArn)},
			Tags:            map[string]string{"shared": "yes"},
		},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("shared tags: %+v %v", tagged, err)
	}
	listed, err := shared.GetResources(
		ctx,
		&resourcegroupstaggingapi.GetResourcesInput{ResourceTypeFilters: []string{"config:config-rule"}},
	)
	if err != nil || len(listed.ResourceTagMappingList) != 1 {
		t.Fatalf("inventory: %+v %v", listed, err)
	}
	_, err = c.StartConfigurationRecorder(
		ctx,
		&sdk.StartConfigurationRecorderInput{ConfigurationRecorderName: new("primary")},
	)
	if err != nil {
		t.Fatal(err)
	}
	status, err := c.DescribeConfigurationRecorderStatus(ctx, &sdk.DescribeConfigurationRecorderStatusInput{})
	if err != nil || len(status.ConfigurationRecordersStatus) != 1 ||
		!status.ConfigurationRecordersStatus[0].Recording ||
		status.ConfigurationRecordersStatus[0].LastStartTime == nil ||
		status.ConfigurationRecordersStatus[0].LastStatus != "" {
		t.Fatalf("requested state without fake capture: %+v %v", status, err)
	}
	delivery, err := c.DescribeDeliveryChannelStatus(ctx, &sdk.DescribeDeliveryChannelStatusInput{})
	if err != nil || len(delivery.DeliveryChannelsStatus) != 1 ||
		delivery.DeliveryChannelsStatus[0].ConfigHistoryDeliveryInfo != nil ||
		delivery.DeliveryChannelsStatus[0].ConfigSnapshotDeliveryInfo != nil ||
		delivery.DeliveryChannelsStatus[0].ConfigStreamDeliveryInfo != nil {
		t.Fatalf("fake delivery history: %+v %v", delivery, err)
	}
	_, err = c.DeleteDeliveryChannel(ctx, &sdk.DeleteDeliveryChannelInput{DeliveryChannelName: new("primary")})
	code(t, err, "LastDeliveryChannelDeleteFailedException")
	_, err = c.StopConfigurationRecorder(
		ctx,
		&sdk.StopConfigurationRecorderInput{ConfigurationRecorderName: new("primary")},
	)
	if err != nil {
		t.Fatal(err)
	}
	status, err = c.DescribeConfigurationRecorderStatus(ctx, &sdk.DescribeConfigurationRecorderStatusInput{})
	if err != nil || status.ConfigurationRecordersStatus[0].Recording ||
		status.ConfigurationRecordersStatus[0].LastStopTime == nil {
		t.Fatalf("stop: %+v %v", status, err)
	}
	for _, other := range []*sdk.Client{configClient(s, "team-b", "us-east-1"), configClient(s, "test", "eu-west-1")} {
		rules, e := other.DescribeConfigRules(ctx, &sdk.DescribeConfigRulesInput{})
		if e != nil || len(rules.ConfigRules) != 0 {
			t.Fatalf("rules leaked: %+v %v", rules, e)
		}
		recorders, e := other.DescribeConfigurationRecorders(ctx, &sdk.DescribeConfigurationRecordersInput{})
		if e != nil || len(recorders.ConfigurationRecorders) != 0 {
			t.Fatalf("recorders leaked: %+v %v", recorders, e)
		}
		channels, e := other.DescribeDeliveryChannels(ctx, &sdk.DescribeDeliveryChannelsInput{})
		if e != nil || len(channels.DeliveryChannels) != 0 {
			t.Fatalf("channels leaked: %+v %v", channels, e)
		}
		_, e = other.TagResource(
			ctx,
			&sdk.TagResourceInput{
				ResourceArn: original.ConfigRuleArn,
				Tags:        []types.Tag{{Key: new("foreign"), Value: new("bad")}},
			},
		)
		code(t, e, "ResourceNotFoundException")
	}
	_, err = c.DeleteConfigRule(ctx, &sdk.DeleteConfigRuleInput{ConfigRuleName: new("required-tags")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DeleteConfigRule(ctx, &sdk.DeleteConfigRuleInput{ConfigRuleName: new("required-tags")})
	code(t, err, "NoSuchConfigRuleException")
	_, err = c.DeleteDeliveryChannel(ctx, &sdk.DeleteDeliveryChannelInput{DeliveryChannelName: new("primary")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DeleteDeliveryChannel(ctx, &sdk.DeleteDeliveryChannelInput{DeliveryChannelName: new("primary")})
	code(t, err, "NoSuchDeliveryChannelException")
	_, err = c.DeleteConfigurationRecorder(
		ctx,
		&sdk.DeleteConfigurationRecorderInput{ConfigurationRecorderName: new("primary")},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DeleteConfigurationRecorder(
		ctx,
		&sdk.DeleteConfigurationRecorderInput{ConfigurationRecorderName: new("primary")},
	)
	code(t, err, "NoSuchConfigurationRecorderException")
}

func TestConfigPaginationAndValidation(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := configClient(s, "test", "us-east-1")
	for i := range 27 {
		if _, err := c.PutConfigRule(
			ctx,
			&sdk.PutConfigRuleInput{ConfigRule: configRule(fmt.Sprintf("rule-%02d", i))},
		); err != nil {
			t.Fatal(err)
		}
	}
	first, err := c.DescribeConfigRules(ctx, &sdk.DescribeConfigRulesInput{})
	if err != nil || len(first.ConfigRules) != 25 || first.NextToken == nil {
		t.Fatalf("first: %+v %v", first, err)
	}
	second, err := c.DescribeConfigRules(ctx, &sdk.DescribeConfigRulesInput{NextToken: first.NextToken})
	if err != nil || len(second.ConfigRules) != 2 || second.NextToken != nil ||
		aws.ToString(second.ConfigRules[0].ConfigRuleName) != "rule-25" {
		t.Fatalf("second: %+v %v", second, err)
	}
	filtered, err := c.DescribeConfigRules(
		ctx,
		&sdk.DescribeConfigRulesInput{ConfigRuleNames: []string{"rule-25", "rule-01"}},
	)
	if err != nil || len(filtered.ConfigRules) != 2 ||
		aws.ToString(filtered.ConfigRules[0].ConfigRuleName) != "rule-01" {
		t.Fatalf("filter order: %+v %v", filtered, err)
	}
	_, err = c.DescribeConfigRules(ctx, &sdk.DescribeConfigRulesInput{ConfigRuleNames: []string{"missing"}})
	code(t, err, "NoSuchConfigRuleException")
	_, err = c.DescribeConfigRules(ctx, &sdk.DescribeConfigRulesInput{NextToken: new("!")})
	code(t, err, "InvalidNextTokenException")
	_, err = c.ListTagsForResource(
		ctx,
		&sdk.ListTagsForResourceInput{ResourceArn: first.ConfigRules[0].ConfigRuleArn, Limit: -1},
	)
	code(t, err, "InvalidLimitException")
	_, err = c.PutConfigurationRecorder(
		ctx,
		&sdk.PutConfigurationRecorderInput{
			ConfigurationRecorder: &types.ConfigurationRecorder{Name: new("primary"), RoleARN: new("")},
		},
	)
	code(t, err, "InvalidRoleException")
	_, err = c.PutConfigurationRecorder(
		ctx,
		&sdk.PutConfigurationRecorderInput{ConfigurationRecorder: configRecorder("primary")},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.PutConfigurationRecorder(
		ctx,
		&sdk.PutConfigurationRecorderInput{ConfigurationRecorder: configRecorder("duplicate")},
	)
	code(t, err, "MaxNumberOfConfigurationRecordersExceededException")
	_, err = s3Client(
		s,
		"test",
		"us-east-1",
	).CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("config-destination")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.PutDeliveryChannel(ctx, &sdk.PutDeliveryChannelInput{DeliveryChannel: configChannel("primary")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.PutDeliveryChannel(ctx, &sdk.PutDeliveryChannelInput{DeliveryChannel: configChannel("duplicate")})
	code(t, err, "MaxNumberOfDeliveryChannelsExceededException")
	bad := configChannel("primary")
	bad.S3BucketName = new("missing-destination")
	_, err = c.PutDeliveryChannel(ctx, &sdk.PutDeliveryChannelInput{DeliveryChannel: bad})
	code(t, err, "NoSuchBucketException")
	bad = configChannel("primary")
	bad.SnsTopicARN = new("arn:aws:sns:eu-west-1:111111111111:foreign")
	_, err = c.PutDeliveryChannel(ctx, &sdk.PutDeliveryChannelInput{DeliveryChannel: bad})
	code(t, err, "InvalidSNSTopicARNException")
	channels, err := c.DescribeDeliveryChannels(ctx, &sdk.DescribeDeliveryChannelsInput{})
	if err != nil || !reflect.DeepEqual(channels.DeliveryChannels[0], *configChannel("primary")) {
		t.Fatalf("malformed channel update changed record: %+v %v", channels, err)
	}
}

func configWire(t *testing.T, s *httptest.Server, operation, body string) (status int, result map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.URL, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "StarlingDoveService."+operation)
	req.Header.Set(
		"Authorization",
		"AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/config/aws4_request, SignedHeaders=host, Signature=dummy",
	)
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatalf("wire body %s: %v", data, err)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-amz-json-1.1") {
		t.Fatalf("content type: %v", resp.Header)
	}
	return resp.StatusCode, result
}

func TestConfigRawWireAtomicityAndUnsupported(t *testing.T) {
	s := server(t)
	c := configClient(s, "test", "us-east-1")
	ctx := context.Background()
	putConfigFixture(t, s, c)
	status, body := configWire(t, s, "DescribeConfigRules", `{}`)
	if status != 200 || len(body["ConfigRules"].([]any)) != 1 {
		t.Fatalf("raw rules: %d %+v", status, body)
	}
	cases := []struct{ op, body, want string }{
		{"PutConfigRule", `{}`, "InvalidParameterValueException"},
		{"PutConfigRule", `{"ConfigRule":{}}`, "InvalidParameterValueException"},
		{
			"PutConfigRule",
			`{"ConfigRule":{"ConfigRuleName":"required-tags","Source":{"Owner":"AWS"}}}`,
			"InvalidParameterValueException",
		},
		{
			"PutConfigRule",
			`{"ConfigRule":{"ConfigRuleName":"required-tags","Source":{"Owner":"AWS","SourceI` +
				`dentifier":"REQUIRED_TAGS","SourceDetails":[]}}}`,
			"ValidationException",
		},
		{
			"PutConfigRule",
			`{"ConfigRule":{"ConfigRuleName":"required-tags","Source":{"Owner":"AWS","SourceI` +
				`dentifier":"REQUIRED_TAGS"},"EvaluationModes":[]}}`,
			"ValidationException",
		},
		{"PutConfigurationRecorder", `{"ConfigurationRecorder":{}}`, "InvalidRoleException"},
		{
			"PutConfigurationRecorder",
			`{"ConfigurationRecorder":{"name":"primary","roleARN":"arn:aws:iam::000000000000:` +
				`role/config-role","recordingGroup":{}}}`,
			"InvalidRecordingGroupException",
		},
		{
			"PutConfigurationRecorder",
			`{"ConfigurationRecorder":{"name":"primary","servicePrincipal":"config.amazonaws.com"}}`,
			"ValidationException",
		},
		{
			"PutConfigurationRecorder",
			`{"ConfigurationRecorder":{"name":"primary","recordingMode":{"recordingFrequency":"DAILY"}}}`,
			"ValidationException",
		},
		{"PutDeliveryChannel", `{"DeliveryChannel":{}}`, "InvalidParameterValueException"},
		{
			"PutDeliveryChannel",
			`{"DeliveryChannel":{"name":"primary","s3BucketName":"config-destination","configSnapshotDeliveryProperties":{}}}`,
			"InvalidParameterValueException",
		},
		{
			"PutDeliveryChannel",
			`{"DeliveryChannel":{"name":"primary","s3KmsKeyArn":"unsupported"}}`,
			"ValidationException",
		},
		{
			"TagResource",
			`{"ResourceArn":"foreign","Tags":[{"Key":"key","Value":"value"}]}`,
			"ResourceNotFoundException",
		},
		{"TagResource", `{"Tags":[]}`, "InvalidParameterValueException"},
		{"StartConfigurationRecorder", `{"ConfigurationRecorderName":""}`, "InvalidParameterValueException"},
		{"DescribeComplianceByConfigRule", `{}`, "UnknownOperationException"},
	}
	beforeRules, _ := c.DescribeConfigRules(ctx, &sdk.DescribeConfigRulesInput{})
	beforeRecorders, _ := c.DescribeConfigurationRecorders(ctx, &sdk.DescribeConfigurationRecordersInput{})
	beforeChannels, _ := c.DescribeDeliveryChannels(ctx, &sdk.DescribeDeliveryChannelsInput{})
	for _, tc := range cases {
		t.Run(tc.op+tc.want+tc.body, func(t *testing.T) {
			httpStatus, result := configWire(t, s, tc.op, tc.body)
			if httpStatus != 400 || result["__type"] != tc.want || result["message"] == nil {
				t.Fatalf("raw error: %d %+v", httpStatus, result)
			}
		})
	}
	afterRules, _ := c.DescribeConfigRules(ctx, &sdk.DescribeConfigRulesInput{})
	afterRecorders, _ := c.DescribeConfigurationRecorders(ctx, &sdk.DescribeConfigurationRecordersInput{})
	afterChannels, _ := c.DescribeDeliveryChannels(ctx, &sdk.DescribeDeliveryChannelsInput{})
	// SDK metadata includes request IDs; compare persisted resource values.
	if !reflect.DeepEqual(beforeRules.ConfigRules, afterRules.ConfigRules) ||
		!reflect.DeepEqual(beforeRecorders.ConfigurationRecorders, afterRecorders.ConfigurationRecorders) ||
		!reflect.DeepEqual(beforeChannels.DeliveryChannels, afterChannels.DeliveryChannels) {
		t.Fatal("malformed write changed records")
	}
}

func TestConfigSigV4Verify(t *testing.T) {
	ctx := context.Background()
	s := verifyingServer(t)
	c := configClient(s, "test", "us-east-1")
	if _, err := c.PutConfigRule(ctx, &sdk.PutConfigRuleInput{ConfigRule: configRule("signed")}); err != nil {
		t.Fatal(err)
	}
	rules, err := c.DescribeConfigRules(ctx, &sdk.DescribeConfigRulesInput{})
	if err != nil || len(rules.ConfigRules) != 1 {
		t.Fatalf("signed config routing: %+v %v", rules, err)
	}
	_, err = configClient(s, "wrong", "us-east-1").DescribeConfigRules(ctx, &sdk.DescribeConfigRulesInput{})
	code(t, err, "InvalidSignatureException")
}

func TestConfigPersistenceAndReset(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "config.db")
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	s, stop := serverStore(t, testConfig(), &fakeRuntime{}, store)
	c := configClient(s, "test", "us-east-1")
	putConfigFixture(t, s, c)
	_, err = c.StartConfigurationRecorder(
		ctx,
		&sdk.StartConfigurationRecorderInput{ConfigurationRecorderName: new("primary")},
	)
	if err != nil {
		t.Fatal(err)
	}
	beforeRule, _ := c.DescribeConfigRules(ctx, &sdk.DescribeConfigRulesInput{})
	beforeRecorder, _ := c.DescribeConfigurationRecorders(ctx, &sdk.DescribeConfigurationRecordersInput{})
	beforeStatus, _ := c.DescribeConfigurationRecorderStatus(ctx, &sdk.DescribeConfigurationRecorderStatusInput{})
	beforeChannel, _ := c.DescribeDeliveryChannels(ctx, &sdk.DescribeDeliveryChannelsInput{})
	stop()
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	s2, stop2 := serverStore(t, testConfig(), &fakeRuntime{}, reopened)
	defer func() { stop2(); _ = reopened.Close() }()
	c2 := configClient(s2, "test", "us-east-1")
	rules, err := c2.DescribeConfigRules(ctx, &sdk.DescribeConfigRulesInput{})
	if err != nil {
		t.Fatal(err)
	}
	recorders, err := c2.DescribeConfigurationRecorders(ctx, &sdk.DescribeConfigurationRecordersInput{})
	if err != nil {
		t.Fatal(err)
	}
	statuses, err := c2.DescribeConfigurationRecorderStatus(ctx, &sdk.DescribeConfigurationRecorderStatusInput{})
	if err != nil {
		t.Fatal(err)
	}
	channels, err := c2.DescribeDeliveryChannels(ctx, &sdk.DescribeDeliveryChannelsInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeRule.ConfigRules, rules.ConfigRules) ||
		!reflect.DeepEqual(beforeRecorder.ConfigurationRecorders, recorders.ConfigurationRecorders) ||
		!reflect.DeepEqual(beforeStatus.ConfigurationRecordersStatus, statuses.ConfigurationRecordersStatus) ||
		!reflect.DeepEqual(beforeChannel.DeliveryChannels, channels.DeliveryChannels) {
		t.Fatal("nondefault persisted configuration or state changed after restart")
	}
	for _, arn := range []*string{rules.ConfigRules[0].ConfigRuleArn, recorders.ConfigurationRecorders[0].Arn} {
		tags, e := c2.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: arn})
		if e != nil || len(tags.Tags) == 0 {
			t.Fatalf("tags lost: %+v %v", tags, e)
		}
	}
	httpStatus, body := post(t, s2, "/_tinystack/reset", nil)
	if httpStatus != 200 {
		t.Fatalf("reset: %d %s", httpStatus, body)
	}
	rules, err = c2.DescribeConfigRules(ctx, &sdk.DescribeConfigRulesInput{})
	if err != nil || len(rules.ConfigRules) != 0 {
		t.Fatalf("reset rules: %+v %v", rules, err)
	}
	recorders, err = c2.DescribeConfigurationRecorders(ctx, &sdk.DescribeConfigurationRecordersInput{})
	if err != nil || len(recorders.ConfigurationRecorders) != 0 {
		t.Fatalf("reset recorders: %+v %v", recorders, err)
	}
	channels, err = c2.DescribeDeliveryChannels(ctx, &sdk.DescribeDeliveryChannelsInput{})
	if err != nil || len(channels.DeliveryChannels) != 0 {
		t.Fatalf("reset channels: %+v %v", channels, err)
	}
}

func TestConfigDefaultsAndExplicitMalformedValues(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := configClient(s, "test", "us-east-1")
	if _, err := s3Client(
		s,
		"test",
		"us-east-1",
	).CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("config-destination")}); err != nil {
		t.Fatal(err)
	}
	channel := configChannel("default")
	channel.Name = nil
	_, err := c.PutDeliveryChannel(ctx, &sdk.PutDeliveryChannelInput{DeliveryChannel: channel})
	code(t, err, "NoAvailableConfigurationRecorderException")
	_, err = c.PutConfigurationRecorder(ctx, &sdk.PutConfigurationRecorderInput{
		ConfigurationRecorder: &types.ConfigurationRecorder{RoleARN: new("arn:aws:iam::000000000000:role/config-role")},
		Tags:                  []types.Tag{{Key: new("empty-value")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	recorders, err := c.DescribeConfigurationRecorders(ctx, &sdk.DescribeConfigurationRecordersInput{})
	if err != nil || len(recorders.ConfigurationRecorders) != 1 ||
		aws.ToString(recorders.ConfigurationRecorders[0].Name) != "default" ||
		!recorders.ConfigurationRecorders[0].RecordingGroup.AllSupported ||
		recorders.ConfigurationRecorders[0].RecordingGroup.IncludeGlobalResourceTypes {
		t.Fatalf("recorder defaults: %+v %v", recorders, err)
	}
	original := recorders.ConfigurationRecorders[0]
	for _, body := range []string{
		`{"ConfigurationRecorder":{"name":"","roleARN":"arn:aws:iam::000000000000:role/config-role"}}`,
		`{"ConfigurationRecorder":{"name":"default","roleARN":"arn:aws:iam::000000000000:role//"}}`,
		`{"ConfigurationRecorder":{"name":"default","roleARN":"arn:aws:iam::000000000000:` +
			`role/config-role","recordingGroup":{"allSupported":false}}}`,
	} {
		status, _ := configWire(t, s, "PutConfigurationRecorder", body)
		if status != 400 {
			t.Fatalf("explicit malformed recorder accepted: %s", body)
		}
	}
	recorders, err = c.DescribeConfigurationRecorders(ctx, &sdk.DescribeConfigurationRecordersInput{})
	if err != nil || !reflect.DeepEqual(original, recorders.ConfigurationRecorders[0]) {
		t.Fatalf("malformed update replaced defaults: %+v %v", recorders, err)
	}
	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: original.Arn})
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Value) != "" {
		t.Fatalf("optional tag value: %+v %v", tags, err)
	}
	if _, err = c.PutDeliveryChannel(ctx, &sdk.PutDeliveryChannelInput{DeliveryChannel: channel}); err != nil {
		t.Fatal(err)
	}
	status, result := configWire(
		t,
		s,
		"PutDeliveryChannel",
		`{"DeliveryChannel":{"name":"","s3BucketName":"config-destination"}}`,
	)
	if status != 400 || result["__type"] != "InvalidDeliveryChannelNameException" {
		t.Fatalf("empty channel name: %d %+v", status, result)
	}
	status, result = configWire(
		t,
		s,
		"ListTagsForResource",
		`{"ResourceArn":"`+aws.ToString(original.Arn)+`","Limit":0}`,
	)
	if status != 400 || result["__type"] != "InvalidLimitException" {
		t.Fatalf("explicit zero limit: %d %+v", status, result)
	}
	// Repeated Start is idempotent, preserving the transition timestamp.
	start := &sdk.StartConfigurationRecorderInput{ConfigurationRecorderName: new("default")}
	if _, err = c.StartConfigurationRecorder(ctx, start); err != nil {
		t.Fatal(err)
	}
	before, err := c.DescribeConfigurationRecorderStatus(ctx, &sdk.DescribeConfigurationRecorderStatusInput{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.StartConfigurationRecorder(ctx, start); err != nil {
		t.Fatal(err)
	}
	after, err := c.DescribeConfigurationRecorderStatus(ctx, &sdk.DescribeConfigurationRecorderStatusInput{})
	if err != nil || !reflect.DeepEqual(before.ConfigurationRecordersStatus, after.ConfigurationRecordersStatus) {
		t.Fatalf("repeated start changed transition state: %+v %v", after, err)
	}
}
