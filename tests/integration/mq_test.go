package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	sdk "github.com/aws/aws-sdk-go-v2/service/mq"
	"github.com/aws/aws-sdk-go-v2/service/mq/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/amjadjibon/tinystack/internal/storage/archive"
)

func mqClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func mqInput() *sdk.CreateBrokerInput {
	return &sdk.CreateBrokerInput{
		BrokerName: new("records-broker"),
		EngineType: types.EngineTypeActivemq,
		EngineVersion: new(
			"5.18",
		),
		HostInstanceType:        new("mq.m5.large"),
		DeploymentMode:          types.DeploymentModeSingleInstance,
		PubliclyAccessible:      new(true),
		AutoMinorVersionUpgrade: new(true),
		CreatorRequestId:        new("mq-sdk-request"),
		Users:                   []types.User{{Username: new("admin"), Password: new("sample-valid-password")}},
		EncryptionOptions:       &types.EncryptionOptions{UseAwsOwnedKey: new(true)},
		Logs: &types.Logs{
			General: new(true),
			Audit:   new(false),
		},
		Tags: map[string]string{"owner": "local"},
		MaintenanceWindowStartTime: &types.WeeklyStartTime{
			DayOfWeek: types.DayOfWeekFriday,
			TimeOfDay: new("12:34"),
			TimeZone:  new("UTC"),
		},
	}
}

func mqBroker(t *testing.T, c *sdk.Client, id *string) *sdk.DescribeBrokerOutput {
	t.Helper()
	b, e := c.DescribeBroker(context.Background(), &sdk.DescribeBrokerInput{BrokerId: id})
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func mqMetadata(b *sdk.DescribeBrokerOutput) *sdk.DescribeBrokerOutput {
	// Transport request IDs vary; the complete modeled metadata must be identical.
	out := *b
	out.ResultMetadata = middleware.Metadata{}
	return &out
}

func mqStatus(t *testing.T, e error, name string, status int) {
	t.Helper()
	code(t, e, name)
	var response *smithyhttp.ResponseError
	if !errors.As(e, &response) || response.HTTPStatusCode() != status {
		t.Fatalf("status %s/%d: %v", name, status, e)
	}
}

func TestMQSDKLifecycleScopeTagsAndIdempotency(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := mqClient(s, "test", "us-east-1")
	in := mqInput()
	created, e := c.CreateBroker(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	before := mqBroker(t, c, created.BrokerId)
	_, records := inspect(t, s, "GET", "/_tinystack/requests")
	found := false
	for _, record := range records {
		if record.Service == "mq" && record.Operation == "CreateBroker" {
			found = strings.Contains(record.RequestBody, "redacted")
			if strings.Contains(record.RequestBody, *in.Users[0].Password) ||
				strings.Contains(record.ResponseBody, *in.Users[0].Password) {
				t.Fatal("MQ diagnostics leaked initial credential")
			}
		}
	}
	if !found {
		t.Fatal("missing redacted MQ create request")
	}

	if before.BrokerState != "" || len(before.BrokerInstances) != 0 || before.Configurations != nil ||
		len(
			before.ActionsRequired,
		) != 0 || before.PendingEngineVersion != nil || before.PendingHostInstanceType != nil ||
		before.Created == nil || before.BrokerId == nil ||
		*before.BrokerArn != "arn:aws:mq:us-east-1:000000000000:broker:records-broker:"+*before.BrokerId ||
		len(before.Users) != 1 || *before.Users[0].Username != "admin" || *before.Logs.Audit || !*before.Logs.General {
		t.Fatal("records boundary or metadata retention")
	}
	replay, e := c.CreateBroker(ctx, in)
	if e != nil || *replay.BrokerId != *created.BrokerId {
		t.Fatal("replay", e)
	}
	changed := mqInput()
	changed.Users[0].Password = new("different-password")
	_, e = c.CreateBroker(ctx, changed)
	mqStatus(t, e, "ConflictException", 409)
	duplicate := mqInput()
	duplicate.CreatorRequestId = new("other-request")
	_, e = c.CreateBroker(ctx, duplicate)
	var collision *types.ConflictException
	if !errors.As(e, &collision) {
		t.Fatal("modeled conflict", e)
	}
	mqStatus(t, e, "ConflictException", 409)
	_, e = c.UpdateBroker(ctx, &sdk.UpdateBrokerInput{
		BrokerId: created.BrokerId, EngineVersion: new("5.19"),
		HostInstanceType: new("mq.t3.micro"), Logs: &types.Logs{General: new(false)},
	})
	if e != nil {
		t.Fatal(e)
	}
	updated := mqBroker(t, c, created.BrokerId)
	if *updated.EngineVersion != "5.19" || *updated.HostInstanceType != "mq.t3.micro" || *updated.Logs.General ||
		*updated.Logs.Audit || !before.Created.Equal(*updated.Created) || *before.BrokerArn != *updated.BrokerArn ||
		*updated.MaintenanceWindowStartTime.TimeOfDay != "12:34" {
		t.Fatal("update retention")
	}
	_, e = c.UpdateBroker(ctx, &sdk.UpdateBrokerInput{BrokerId: created.BrokerId, AutoMinorVersionUpgrade: new(false)})
	mqStatus(t, e, "BadRequestException", 400)
	if !reflect.DeepEqual(mqMetadata(updated), mqMetadata(mqBroker(t, c, created.BrokerId))) {
		t.Fatal("failed update persisted")
	}
	_, e = c.DescribeBroker(ctx, &sdk.DescribeBrokerInput{BrokerId: created.BrokerArn})
	mqStatus(t, e, "BadRequestException", 400)
	_, e = c.CreateTags(
		ctx,
		&sdk.CreateTagsInput{ResourceArn: created.BrokerArn, Tags: map[string]string{"owner": "merged", "empty": ""}},
	)
	if e != nil {
		t.Fatal(e)
	}
	tags, e := c.ListTags(ctx, &sdk.ListTagsInput{ResourceArn: created.BrokerArn})
	if e != nil || tags.Tags["owner"] != "merged" || len(tags.Tags) != 2 {
		t.Fatal("tags", e)
	}
	_, e = c.DeleteTags(ctx, &sdk.DeleteTagsInput{ResourceArn: created.BrokerArn, TagKeys: []string{"empty", "absent"}})
	if e != nil {
		t.Fatal(e)
	}
	tagger := resourcegroupstaggingapi.New(
		resourcegroupstaggingapi.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	tagged, e := tagger.TagResources(ctx, &resourcegroupstaggingapi.TagResourcesInput{
		ResourceARNList: []string{*created.BrokerArn}, Tags: map[string]string{"external": "retained"},
	})
	if e != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatal("resource groups tagging", tagged, e)
	}
	untagged, e := tagger.UntagResources(ctx, &resourcegroupstaggingapi.UntagResourcesInput{
		ResourceARNList: []string{*created.BrokerArn}, TagKeys: []string{"external"},
	})
	if e != nil || len(untagged.FailedResourcesMap) != 0 {
		t.Fatal("resource groups untagging", untagged, e)
	}
	for _, other := range []*sdk.Client{mqClient(s, "team-b", "us-east-1"), mqClient(s, "test", "us-west-2")} {
		_, e = other.DescribeBroker(ctx, &sdk.DescribeBrokerInput{BrokerId: created.BrokerId})
		mqStatus(t, e, "NotFoundException", 404)
		_, e = other.UpdateBroker(ctx, &sdk.UpdateBrokerInput{BrokerId: created.BrokerId})
		mqStatus(t, e, "NotFoundException", 404)
		_, e = other.DeleteBroker(ctx, &sdk.DeleteBrokerInput{BrokerId: created.BrokerId})
		mqStatus(t, e, "NotFoundException", 404)
		_, e = other.ListTags(ctx, &sdk.ListTagsInput{ResourceArn: created.BrokerArn})
		mqStatus(t, e, "NotFoundException", 404)
		list, listErr := other.ListBrokers(ctx, &sdk.ListBrokersInput{})
		if listErr != nil || len(list.BrokerSummaries) != 0 {
			t.Fatal("scope list", listErr)
		}
	}
	listed, e := c.ListBrokers(ctx, &sdk.ListBrokersInput{MaxResults: new(int32(5))})
	if e != nil || len(listed.BrokerSummaries) != 1 || *listed.BrokerSummaries[0].BrokerId != *created.BrokerId ||
		listed.BrokerSummaries[0].BrokerState != "" {
		t.Fatal("list", e)
	}
	deleted, e := c.DeleteBroker(ctx, &sdk.DeleteBrokerInput{BrokerId: created.BrokerId})
	if e != nil || *deleted.BrokerId != *created.BrokerId {
		t.Fatal("delete", e)
	}
	_, e = c.DeleteBroker(ctx, &sdk.DeleteBrokerInput{BrokerId: created.BrokerId})
	var missing *types.NotFoundException
	if !errors.As(e, &missing) {
		t.Fatal("modeled missing", e)
	}
	mqStatus(t, e, "NotFoundException", 404)
	_, e = c.CreateBroker(ctx, in)
	mqStatus(t, e, "ConflictException", 409)
}

func mqWire(
	t *testing.T,
	s *httptest.Server,
	method, path, body string,
) (status int, response map[string]any, bodyBytes []byte) {
	t.Helper()
	req, e := http.NewRequestWithContext(context.Background(), method, s.URL+path, bytes.NewBufferString(body))
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(
		"Authorization",
		"AWS4-HMAC-SHA256 Credential=test/20261011/us-east-1/mq/aws4_request, SignedHeaders=host, Signature=dummy",
	)
	resp, e := s.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(resp.Body)
	if e != nil {
		t.Fatal(e)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if e = json.Unmarshal(raw, &out); e != nil {
			t.Fatal("response is not JSON", e)
		}
	}
	return resp.StatusCode, out, raw
}

func TestMQWirePinnedRoutesCaseNestedExclusionsAndErrors(t *testing.T) {
	s := server(t)
	body := `{"brokerName":"wire","engineType":"RABBITMQ","engineVersion":"3.13",
"hostInstanceType":"mq.m5.large","deploymentMode":"SINGLE_INSTANCE","publiclyAccessible":true,
"users":[{"username":"admin","password":"sample-valid-password"}],"logs":{"general":false},
"creatorRequestId":"wire-request"}`
	status, rejected, _ := mqWire(t, s, "POST", "/v1/brokers",
		strings.Replace(body, "mq.m5.large", "mq.t3.micro", 1))
	if status != 400 || rejected["message"] == nil ||
		!strings.Contains(rejected["__type"].(string), "BadRequestException") {
		t.Fatal("deprecated RabbitMQ create error/status/body", status)
	}
	status, listed, _ := mqWire(t, s, "GET", "/v1/brokers", "")
	if status != 200 || listed["brokerSummaries"] != nil {
		t.Fatal("rejected wire creation left a broker", status)
	}
	// The valid request reuses the rejected name/token, proving neither was reserved.
	status, out, _ := mqWire(t, s, "POST", "/v1/brokers", body)
	if status != 200 || out["brokerId"] == nil || out["BrokerId"] != nil {
		t.Fatal("create route/case", status)
	}
	id := out["brokerId"].(string)
	arn := out["brokerArn"].(string)
	status, out, raw := mqWire(t, s, "GET", "/v1/brokers/"+id, "")
	if status != 200 || out["engineType"] != "RABBITMQ" || out["brokerState"] != nil || out["brokerInstances"] != nil ||
		out["users"] != nil || strings.Contains(string(raw), "password") {
		t.Fatal("describe route/runtime/secrecy", status)
	}
	before := append([]byte(nil), raw...)
	status, rejected, _ = mqWire(t, s, "PUT", "/v1/brokers/"+id,
		`{"hostInstanceType":"mq.t3.micro","logs":{"general":true}}`)
	if status != 400 || rejected["message"] == nil ||
		!strings.Contains(rejected["__type"].(string), "BadRequestException") {
		t.Fatal("RabbitMQ downgrade error/status/body", status)
	}
	status, _, raw = mqWire(t, s, "GET", "/v1/brokers/"+id, "")
	if status != 200 || !bytes.Equal(before, raw) {
		t.Fatal("rejected wire downgrade changed exact metadata", status)
	}
	for _, extra := range []string{
		`"authenticationStrategy":"SIMPLE"`, `"ldapServerMetadata":{}`,
		`"storageType":""`, `"storageSize":0`, `"dataReplicationMode":"NONE"`, `"dataReplicationPrimaryBrokerArn":""`,
		`"users":[{"username":"admin","password":"sample-valid-password","replicationUser":null}]`,
		`"encryptionOptions":{"useAwsOwnedKey":true,"kmsKeyId":""}`,
		`"logs":{"general":false,"pending":null}`,
	} {
		status, _, _ = mqWire(t, s, "POST", "/v1/brokers", "{"+extra+"}")
		if status != 501 {
			t.Fatal("unsupported creation member accepted", status)
		}
	}
	for _, bad := range []string{
		`{"authenticationStrategy":null}`, `{"ldapServerMetadata":null}`,
		`{"resourceShareArns":[]}`, `{"dataReplicationMode":""}`, `{"storageSize":0}`, `{"brokerName":"changed"}`,
		`{"logs":{"audit":false}}`, `{"logs":{"general":false,"generalLogGroup":""}}`,
		`{"maintenanceWindowStartTime":{"dayOfWeek":"MONDAY","timeOfDay":"00:00","unexpected":null}}`,
	} {
		status, _, _ = mqWire(t, s, "PUT", "/v1/brokers/"+id, bad)
		want := 501
		if bad == `{"logs":{"audit":false}}` {
			want = 400
		}
		if status != want {
			t.Fatal("unsupported update accepted", status)
		}
	}
	status, out, _ = mqWire(t, s, "PUT", "/v1/brokers/"+id, `{"logs":{"general":true}}`)
	if status != 200 || out["logs"].(map[string]any)["general"] != true || out["brokerId"] != id {
		t.Fatal("update path/body case")
	}
	status, out, _ = mqWire(t, s, "GET", "/v1/brokers?maxResults=5&nextToken=", "")
	if status != 200 || len(out["brokerSummaries"].([]any)) != 1 {
		t.Fatal("list query")
	}
	for _, query := range []string{"maxResults=4", "MaxResults=5", "maxResults=5&maxResults=6", "unknown=false"} {
		status, _, _ = mqWire(t, s, "GET", "/v1/brokers?"+query, "")
		want := 400
		if query == "unknown=false" || query == "MaxResults=5" {
			want = 501
		}
		if status != want {
			t.Fatal("bad query accepted", query, status)
		}
	}
	path := "/v1/tags/" + url.PathEscape(arn)
	status, _, raw = mqWire(t, s, "POST", path, `{"tags":{"a":"","b":"keep"}}`)
	if status != 204 || len(raw) != 0 {
		t.Fatal("tag unit status/body", status)
	}
	status, out, _ = mqWire(t, s, "GET", path, "")
	if status != 200 || len(out["tags"].(map[string]any)) != 2 {
		t.Fatal("tag GET")
	}
	status, _, raw = mqWire(t, s, "DELETE", path+"?tagKeys=a&tagKeys=absent", "")
	if status != 204 || len(raw) != 0 {
		t.Fatal("repeated tagKeys query binding")
	}
	status, out, _ = mqWire(t, s, "GET", path, "")
	if status != 200 || len(out["tags"].(map[string]any)) != 1 {
		t.Fatal("tag removal")
	}
	for _, route := range []struct{ method, path string }{
		{"PATCH", "/v1/brokers/" + id},
		{"POST", "/v1/configurations"},
		{"GET", "/v1/brokers/" + id + "/users"},
	} {
		status, _, _ = mqWire(t, s, route.method, route.path, `{}`)
		if status >= 200 && status < 300 {
			t.Fatal("unsupported route succeeded")
		}
	}
	status, out, _ = mqWire(t, s, "DELETE", "/v1/brokers/"+id, "")
	if status != 200 || out["brokerId"] != id {
		t.Fatal("delete route")
	}
	status, out, _ = mqWire(t, s, "GET", "/v1/brokers/"+id, "")
	if status != 404 || out["message"] == nil || !strings.Contains(out["__type"].(string), "NotFoundException") {
		t.Fatal("modeled error body", status)
	}
}

func TestMQVerifySignatures(t *testing.T) {
	s := verifyingServer(t)
	ctx := context.Background()
	c := mqClient(s, "test", "us-east-1")
	created, e := c.CreateBroker(ctx, mqInput())
	if e != nil {
		t.Fatal(e)
	}
	mqBroker(t, c, created.BrokerId)
	if _, e = c.ListBrokers(ctx, &sdk.ListBrokersInput{}); e != nil {
		t.Fatal(e)
	}
	if _, e = c.UpdateBroker(
		ctx,
		&sdk.UpdateBrokerInput{BrokerId: created.BrokerId, Logs: &types.Logs{General: new(false)}},
	); e != nil {
		t.Fatal(e)
	}
	if _, e = c.CreateTags(
		ctx,
		&sdk.CreateTagsInput{ResourceArn: created.BrokerArn, Tags: map[string]string{"signed": "yes"}},
	); e != nil {
		t.Fatal(e)
	}
	if _, e = c.ListTags(ctx, &sdk.ListTagsInput{ResourceArn: created.BrokerArn}); e != nil {
		t.Fatal(e)
	}
	if _, e = c.DeleteTags(
		ctx,
		&sdk.DeleteTagsInput{ResourceArn: created.BrokerArn, TagKeys: []string{"signed"}},
	); e != nil {
		t.Fatal(e)
	}
	_, e = mqClient(s, "wrong", "us-east-1").ListBrokers(ctx, &sdk.ListBrokersInput{})
	code(t, e, "InvalidSignatureException")
	if _, e = c.DeleteBroker(ctx, &sdk.DeleteBrokerInput{BrokerId: created.BrokerId}); e != nil {
		t.Fatal(e)
	}
}

func TestMQSQLiteRestartFixtureExportImportResetAndSecrecy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := mqClient(s, "test", "us-east-1")
	in := mqInput()
	created, e := c.CreateBroker(ctx, in)
	if e != nil {
		stop()
		t.Fatal(e)
	}
	if _, e = c.UpdateBroker(ctx, &sdk.UpdateBrokerInput{
		BrokerId: created.BrokerId, EngineVersion: new("5.19"),
		Logs: &types.Logs{General: new(false), Audit: new(true)}, MaintenanceWindowStartTime: &types.WeeklyStartTime{
			DayOfWeek: types.DayOfWeekTuesday, TimeOfDay: new("23:59"), TimeZone: new("UTC"),
		},
	}); e != nil {
		stop()
		t.Fatal(e)
	}
	if _, e = c.CreateTags(
		ctx,
		&sdk.CreateTagsInput{ResourceArn: created.BrokerArn, Tags: map[string]string{"snapshot": "exact"}},
	); e != nil {
		stop()
		t.Fatal(e)
	}
	before := mqMetadata(mqBroker(t, c, created.BrokerId))
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	c = mqClient(s, "test", "us-east-1")
	if !reflect.DeepEqual(before, mqMetadata(mqBroker(t, c, created.BrokerId))) {
		stop()
		t.Fatal("SQLite exact metadata lost")
	}
	replay, e := c.CreateBroker(ctx, in)
	if e != nil || *replay.BrokerId != *created.BrokerId {
		stop()
		t.Fatal("restart replay lost", e)
	}
	stop()
	fixture := filepath.Join(t.TempDir(), "mq.tar.gz")
	if e = archive.Export(ctx, archive.ExportOptions{StorageDir: dir, OutputFile: fixture}); e != nil {
		t.Fatal(e)
	}
	child := filepath.Join(t.TempDir(), "child")
	if e = archive.Import(ctx, archive.ImportOptions{StorageDir: child, InputFile: fixture}); e != nil {
		t.Fatal(e)
	}
	s, stop = persistent(t, child, &fakeRuntime{})
	c = mqClient(s, "test", "us-east-1")
	if !reflect.DeepEqual(before, mqMetadata(mqBroker(t, c, created.BrokerId))) {
		stop()
		t.Fatal("import exact metadata lost")
	}
	stop()
	s, stop = persistent(t, child, &fakeRuntime{})
	defer stop()
	c = mqClient(s, "test", "us-east-1")
	if !reflect.DeepEqual(before, mqMetadata(mqBroker(t, c, created.BrokerId))) {
		t.Fatal("normal imported restart lost metadata")
	}
	replay, e = c.CreateBroker(ctx, in)
	if e != nil || *replay.BrokerId != *created.BrokerId {
		t.Fatal("imported idempotency lost", e)
	}
	resp, e := s.Client().Get(s.URL + "/_tinystack/resources")
	if e != nil {
		t.Fatal(e)
	}
	raw, e := io.ReadAll(resp.Body)
	resp.Body.Close()
	if e != nil || !strings.Contains(string(raw), *created.BrokerArn) ||
		!strings.Contains(string(raw), "RecordsOnly") ||
		!strings.Contains(string(raw), "ExecutionReady") ||
		strings.Contains(string(raw), *in.Users[0].Password) {
		t.Fatal("inventory boundary or credential leakage")
	}
	status, _ := post(t, s, "/_tinystack/reset", nil)
	if status != 200 {
		t.Fatal("reset")
	}
	list, e := c.ListBrokers(ctx, &sdk.ListBrokersInput{})
	if e != nil || len(list.BrokerSummaries) != 0 {
		t.Fatal("reset retained brokers")
	}
	resp, e = s.Client().Get(s.URL + "/_tinystack/resources")
	if e != nil {
		t.Fatal(e)
	}
	raw, e = io.ReadAll(resp.Body)
	resp.Body.Close()
	if e != nil || strings.Contains(string(raw), *created.BrokerArn) {
		t.Fatal("reset inventory retained")
	}
	recreated, e := c.CreateBroker(ctx, in)
	if e != nil || *recreated.BrokerId == *created.BrokerId {
		t.Fatal("reset retained replay state", e)
	}
}

func TestMQSDKPaginationDeletedCursorAndScopeBoundToken(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := mqClient(s, "test", "us-east-1")
	for i := range 12 {
		in := mqInput()
		in.BrokerName = new(fmt.Sprintf("page-%02d", i))
		in.CreatorRequestId = new(fmt.Sprintf("page-token-%02d", i))
		if _, e := c.CreateBroker(ctx, in); e != nil {
			t.Fatal(e)
		}
	}
	first, e := c.ListBrokers(ctx, &sdk.ListBrokersInput{MaxResults: new(int32(5))})
	if e != nil || len(first.BrokerSummaries) != 5 || first.NextToken == nil {
		t.Fatal("SDK first page", e)
	}
	cursor := first.BrokerSummaries[4].BrokerId
	if _, e = c.DeleteBroker(ctx, &sdk.DeleteBrokerInput{BrokerId: cursor}); e != nil {
		t.Fatal(e)
	}
	second, e := c.ListBrokers(ctx, &sdk.ListBrokersInput{MaxResults: new(int32(5)), NextToken: first.NextToken})
	if e != nil || len(second.BrokerSummaries) != 5 || second.NextToken == nil {
		t.Fatal("SDK deleted cursor resume", e)
	}
	for _, b := range second.BrokerSummaries {
		if *b.BrokerId <= *cursor {
			t.Fatal("SDK pagination repeated a prior ID")
		}
	}
	last, e := c.ListBrokers(ctx, &sdk.ListBrokersInput{NextToken: second.NextToken})
	if e != nil || len(last.BrokerSummaries) != 2 || last.NextToken != nil {
		t.Fatal("SDK terminal page", e)
	}
	for _, other := range []*sdk.Client{mqClient(s, "team-b", "us-east-1"), mqClient(s, "test", "us-west-2")} {
		_, err := other.ListBrokers(ctx, &sdk.ListBrokersInput{NextToken: first.NextToken})
		mqStatus(t, err, "BadRequestException", 400)
	}
	_, e = c.ListBrokers(ctx, &sdk.ListBrokersInput{NextToken: new("invalid")})
	mqStatus(t, e, "BadRequestException", 400)
}

func TestMQSDKPrivateNetworkFalseFlagRetentionAndScope(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	net := ec2Client(s, "test", "us-east-1")
	c := mqClient(s, "test", "us-east-1")
	v, e := net.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: new("10.93.0.0/16")})
	if e != nil {
		t.Fatal(e)
	}
	subnet, e := net.CreateSubnet(ctx, &ec2.CreateSubnetInput{VpcId: v.Vpc.VpcId, CidrBlock: new("10.93.1.0/24")})
	if e != nil {
		t.Fatal(e)
	}
	group, e := net.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		VpcId:     v.Vpc.VpcId,
		GroupName: new("mq-private"), Description: new("metadata"),
	})
	if e != nil {
		t.Fatal(e)
	}
	in := mqInput()
	in.PubliclyAccessible = new(false)
	in.SubnetIds = []string{*subnet.Subnet.SubnetId}
	in.SecurityGroups = []string{*group.GroupId}
	in.Logs.General = new(false)
	created, e := c.CreateBroker(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	b := mqBroker(t, c, created.BrokerId)
	if b.PubliclyAccessible == nil || *b.PubliclyAccessible || *b.Logs.General ||
		!reflect.DeepEqual(in.SubnetIds, b.SubnetIds) || !reflect.DeepEqual(in.SecurityGroups, b.SecurityGroups) {
		t.Fatal("private metadata or false flags lost")
	}
	_, e = mqClient(s, "team-b", "us-east-1").CreateBroker(ctx, in)
	mqStatus(t, e, "BadRequestException", 400)
	_, e = c.UpdateBroker(
		ctx,
		&sdk.UpdateBrokerInput{BrokerId: created.BrokerId, SecurityGroups: []string{"sg-12345678"}},
	)
	mqStatus(t, e, "BadRequestException", 400)
	if !reflect.DeepEqual(mqMetadata(b), mqMetadata(mqBroker(t, c, created.BrokerId))) {
		t.Fatal("invalid private update changed metadata")
	}
}

func TestMQUsersAndConfigurations(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := mqClient(s, "test", "us-east-1")
	config, err := c.CreateConfiguration(
		ctx,
		&sdk.CreateConfigurationInput{
			Name:          new("custom.config~" + strings.Repeat("a", 100)),
			EngineType:    types.EngineTypeActivemq,
			EngineVersion: new("5.18"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	described, err := c.DescribeConfiguration(ctx, &sdk.DescribeConfigurationInput{ConfigurationId: config.Id})
	if err != nil || *described.LatestRevision.Revision != 1 {
		t.Fatal(described, err)
	}
	listed, err := c.ListConfigurations(ctx, &sdk.ListConfigurationsInput{})
	if err != nil || len(listed.Configurations) != 1 {
		t.Fatal(listed, err)
	}
	data := "PGJyb2tlciB4bWxucz0iaHR0cDovL2FjdGl2ZW1xLmFwYWNoZS5vcmcvc2NoZW1hL2NvcmUiLz4="
	updated, err := c.UpdateConfiguration(
		ctx,
		&sdk.UpdateConfigurationInput{ConfigurationId: config.Id, Data: new(data), Description: new("custom XML")},
	)
	if err != nil || *updated.LatestRevision.Revision != 2 {
		t.Fatal(updated, err)
	}
	rev, err := c.DescribeConfigurationRevision(
		ctx,
		&sdk.DescribeConfigurationRevisionInput{ConfigurationId: config.Id, ConfigurationRevision: new("2")},
	)
	if err != nil || *rev.Data != data {
		t.Fatal(rev, err)
	}
	revisions, err := c.ListConfigurationRevisions(
		ctx,
		&sdk.ListConfigurationRevisionsInput{ConfigurationId: config.Id},
	)
	if err != nil || len(revisions.Revisions) != 2 {
		t.Fatal(revisions, err)
	}
	_, err = c.UpdateConfiguration(ctx, &sdk.UpdateConfigurationInput{ConfigurationId: config.Id, Data: new("invalid")})
	mqStatus(t, err, "BadRequestException", 400)
	in := mqInput()
	in.Configuration = &types.ConfigurationId{Id: config.Id, Revision: new(int32(1))}
	b, err := c.CreateBroker(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.UpdateBroker(
		ctx,
		&sdk.UpdateBrokerInput{
			BrokerId:      b.BrokerId,
			Configuration: &types.ConfigurationId{Id: config.Id, Revision: new(int32(2))},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	before := mqBroker(t, c, b.BrokerId)
	if *before.Configurations.Current.Revision != 1 || *before.Configurations.Pending.Revision != 2 {
		t.Fatal(before.Configurations)
	}
	_, err = c.CreateUser(
		ctx,
		&sdk.CreateUserInput{
			BrokerId:      b.BrokerId,
			Username:      new("second"),
			Password:      new("valid-password-123"),
			ConsoleAccess: new(true),
			Groups:        []string{"admins"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	u, err := c.DescribeUser(ctx, &sdk.DescribeUserInput{BrokerId: b.BrokerId, Username: new("second")})
	if err != nil || u.Pending == nil || u.Pending.PendingChange != types.ChangeTypeCreate || u.ConsoleAccess != nil {
		t.Fatal(u, err)
	}
	users, err := c.ListUsers(ctx, &sdk.ListUsersInput{BrokerId: b.BrokerId})
	if err != nil || len(users.Users) != 2 {
		t.Fatal(users, err)
	}
	_, err = c.RebootBroker(ctx, &sdk.RebootBrokerInput{BrokerId: b.BrokerId})
	if err != nil {
		t.Fatal(err)
	}
	after := mqBroker(t, c, b.BrokerId)
	if *after.Configurations.Current.Revision != 2 || after.Configurations.Pending != nil ||
		len(after.Configurations.History) != 1 {
		t.Fatal(after.Configurations)
	}
	u, err = c.DescribeUser(ctx, &sdk.DescribeUserInput{BrokerId: b.BrokerId, Username: new("second")})
	if err != nil || u.Pending != nil || !*u.ConsoleAccess {
		t.Fatal(u, err)
	}
	_, err = c.UpdateUser(
		ctx,
		&sdk.UpdateUserInput{
			BrokerId:      b.BrokerId,
			Username:      new("second"),
			ConsoleAccess: new(false),
			Groups:        []string{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	u, err = c.DescribeUser(ctx, &sdk.DescribeUserInput{BrokerId: b.BrokerId, Username: new("second")})
	if err != nil || u.Pending.PendingChange != types.ChangeTypeUpdate || !*u.ConsoleAccess ||
		*u.Pending.ConsoleAccess {
		t.Fatal(u, err)
	}
	_, err = c.RebootBroker(ctx, &sdk.RebootBrokerInput{BrokerId: b.BrokerId})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DeleteUser(ctx, &sdk.DeleteUserInput{BrokerId: b.BrokerId, Username: new("second")})
	if err != nil {
		t.Fatal(err)
	}
	users, err = c.ListUsers(ctx, &sdk.ListUsersInput{BrokerId: b.BrokerId})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, user := range users.Users {
		if *user.Username == "second" {
			found = user.PendingChange == types.ChangeTypeDelete
		}
	}
	if !found {
		t.Fatal(users)
	}
	_, err = c.RebootBroker(ctx, &sdk.RebootBrokerInput{BrokerId: b.BrokerId})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DescribeUser(ctx, &sdk.DescribeUserInput{BrokerId: b.BrokerId, Username: new("second")})
	mqStatus(t, err, "NotFoundException", 404)
	for _, pw := range []string{
		"short", "aaaaaaaaaaaa", "has:colon-password", "has,comma-password", "has=equals-password",
	} {
		_, err = c.CreateUser(
			ctx,
			&sdk.CreateUserInput{BrokerId: b.BrokerId, Username: new("invalid"), Password: new(pw)},
		)
		mqStatus(t, err, "BadRequestException", 400)
	}
	for _, other := range []*sdk.Client{mqClient(s, "team-b", "us-east-1"), mqClient(s, "test", "us-west-2")} {
		_, err = other.DescribeConfiguration(ctx, &sdk.DescribeConfigurationInput{ConfigurationId: config.Id})
		mqStatus(t, err, "NotFoundException", 404)
		_, err = other.ListUsers(ctx, &sdk.ListUsersInput{BrokerId: b.BrokerId})
		mqStatus(t, err, "NotFoundException", 404)
	}
	_, err = c.DeleteConfiguration(ctx, &sdk.DeleteConfigurationInput{ConfigurationId: config.Id})
	mqStatus(t, err, "ConflictException", 409)
	_, err = c.DeleteBroker(ctx, &sdk.DeleteBrokerInput{BrokerId: b.BrokerId})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DeleteConfiguration(ctx, &sdk.DeleteConfigurationInput{ConfigurationId: config.Id})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DescribeConfiguration(ctx, &sdk.DescribeConfigurationInput{ConfigurationId: config.Id})
	mqStatus(t, err, "NotFoundException", 404)
	rabbit := mqInput()
	rabbit.BrokerName = new("rabbit-users")
	rabbit.CreatorRequestId = nil
	rabbit.EngineType = types.EngineTypeRabbitmq
	rabbit.EngineVersion = new("3.13")
	rabbit.Logs.Audit = nil
	rb, err := c.CreateBroker(ctx, rabbit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ListUsers(ctx, &sdk.ListUsersInput{BrokerId: rb.BrokerId})
	mqStatus(t, err, "BadRequestException", 400)
	_, err = c.DescribeUser(ctx, &sdk.DescribeUserInput{BrokerId: rb.BrokerId, Username: new("admin")})
	mqStatus(t, err, "BadRequestException", 400)
	_, err = c.CreateUser(
		ctx,
		&sdk.CreateUserInput{BrokerId: rb.BrokerId, Username: new("newuser"), Password: new("valid-password-123")},
	)
	mqStatus(t, err, "BadRequestException", 400)
	_, err = c.UpdateUser(
		ctx,
		&sdk.UpdateUserInput{BrokerId: rb.BrokerId, Username: new("admin"), Password: new("valid-password-123")},
	)
	mqStatus(t, err, "BadRequestException", 400)
	_, err = c.DeleteUser(ctx, &sdk.DeleteUserInput{BrokerId: rb.BrokerId, Username: new("admin")})
	mqStatus(t, err, "BadRequestException", 400)
}

func TestMQUserAndConfigurationPagination(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := mqClient(s, "test", "us-east-1")
	b, err := c.CreateBroker(ctx, mqInput())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		_, err = c.CreateUser(
			ctx,
			&sdk.CreateUserInput{
				BrokerId: b.BrokerId,
				Username: new(fmt.Sprintf("user-%d", i)),
				Password: new("valid-password-123"),
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.CreateConfiguration(
			ctx,
			&sdk.CreateConfigurationInput{
				Name:          new(fmt.Sprintf("config-%d", i)),
				EngineType:    types.EngineTypeActivemq,
				EngineVersion: new("5.18"),
			},
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	first, err := c.ListUsers(ctx, &sdk.ListUsersInput{BrokerId: b.BrokerId, MaxResults: new(int32(5))})
	if err != nil || len(first.Users) != 5 || first.NextToken == nil {
		t.Fatal(first, err)
	}
	last, err := c.ListUsers(ctx, &sdk.ListUsersInput{BrokerId: b.BrokerId, NextToken: first.NextToken})
	if err != nil || len(last.Users) != 2 || last.NextToken != nil {
		t.Fatal(last, err)
	}
	configs, err := c.ListConfigurations(ctx, &sdk.ListConfigurationsInput{MaxResults: new(int32(5))})
	if err != nil || len(configs.Configurations) != 5 || configs.NextToken == nil {
		t.Fatal(configs, err)
	}
	tail, err := c.ListConfigurations(ctx, &sdk.ListConfigurationsInput{NextToken: configs.NextToken})
	if err != nil || len(tail.Configurations) != 1 || tail.NextToken != nil {
		t.Fatal(tail, err)
	}
	_, err = c.ListConfigurations(ctx, &sdk.ListConfigurationsInput{NextToken: first.NextToken})
	mqStatus(t, err, "BadRequestException", 400)
	id := configs.Configurations[0].Id
	for range 5 {
		_, err = c.UpdateConfiguration(
			ctx,
			&sdk.UpdateConfigurationInput{ConfigurationId: id, Data: new("PGJyb2tlci8+")},
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	revisions, err := c.ListConfigurationRevisions(
		ctx,
		&sdk.ListConfigurationRevisionsInput{ConfigurationId: id, MaxResults: new(int32(5))},
	)
	if err != nil || len(revisions.Revisions) != 5 || revisions.NextToken == nil {
		t.Fatal(revisions, err)
	}
	end, err := c.ListConfigurationRevisions(
		ctx,
		&sdk.ListConfigurationRevisionsInput{ConfigurationId: id, NextToken: revisions.NextToken},
	)
	if err != nil || len(end.Revisions) != 1 || end.NextToken != nil {
		t.Fatal(end, err)
	}
}

func TestMQConfigurationValidationAndRabbitData(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := mqClient(s, "test", "us-east-1")
	config, err := c.CreateConfiguration(
		ctx,
		&sdk.CreateConfigurationInput{
			Name:          new("rabbit.conf"),
			EngineType:    types.EngineTypeRabbitmq,
			EngineVersion: new("3.13"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	data := "IyBSYWJiaXRNUQpjb25zdW1lcl90aW1lb3V0ID0gMTgwMDAwMAo="
	_, err = c.UpdateConfiguration(ctx, &sdk.UpdateConfigurationInput{ConfigurationId: config.Id, Data: new(data)})
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.CreateBroker(ctx, mqInput())
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.UpdateBroker(
		ctx,
		&sdk.UpdateBrokerInput{
			BrokerId:      b.BrokerId,
			Configuration: &types.ConfigurationId{Id: config.Id, Revision: new(int32(1))},
		},
	)
	mqStatus(t, err, "BadRequestException", 400)
	active, err := c.CreateConfiguration(
		ctx,
		&sdk.CreateConfigurationInput{
			Name:          new("xml"),
			EngineType:    types.EngineTypeActivemq,
			EngineVersion: new("5.18"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"YmFk", "PGJyb2tlcj4=", "PG90aGVyLz4="} {
		_, err = c.UpdateConfiguration(ctx, &sdk.UpdateConfigurationInput{ConfigurationId: active.Id, Data: new(data)})
		mqStatus(t, err, "BadRequestException", 400)
	}
	_, err = c.UpdateBroker(
		ctx,
		&sdk.UpdateBrokerInput{
			BrokerId:      b.BrokerId,
			Configuration: &types.ConfigurationId{Id: active.Id, Revision: new(int32(2))},
		},
	)
	mqStatus(t, err, "BadRequestException", 400)
	_, err = c.DescribeConfigurationRevision(
		ctx,
		&sdk.DescribeConfigurationRevisionInput{ConfigurationId: active.Id, ConfigurationRevision: new("2")},
	)
	mqStatus(t, err, "NotFoundException", 404)
	for _, name := range []string{"a", "has space", "has:colon"} {
		_, err = c.CreateUser(
			ctx,
			&sdk.CreateUserInput{BrokerId: b.BrokerId, Username: new(name), Password: new("valid-password-123")},
		)
		mqStatus(t, err, "BadRequestException", 400)
	}
	_, err = c.CreateUser(
		ctx,
		&sdk.CreateUserInput{
			BrokerId: b.BrokerId,
			Username: new("newuser"),
			Password: new("valid-password-123"),
			Groups:   []string{"a"},
		},
	)
	mqStatus(t, err, "BadRequestException", 400)
	_, err = c.UpdateUser(
		ctx,
		&sdk.UpdateUserInput{BrokerId: b.BrokerId, Username: new("admin"), ReplicationUser: new(false)},
	)
	mqStatus(t, err, "NotImplemented", 501)
}
