package integration_test

import (
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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	sdk "github.com/aws/aws-sdk-go-v2/service/kafka"
	"github.com/aws/aws-sdk-go-v2/service/kafka/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/amjadjibon/tinystack/internal/storage/archive"
)

func mskClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func mskInput(t *testing.T, s *httptest.Server, region string) *sdk.CreateClusterInput {
	t.Helper()
	ctx := context.Background()
	net := ec2Client(s, "test", region)
	v, e := net.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: new("10.94.0.0/16")})
	if e != nil {
		t.Fatal(e)
	}
	ids := []string{}
	for i, az := range []string{"a", "b"} {
		sn, err := net.CreateSubnet(
			ctx,
			&ec2.CreateSubnetInput{
				VpcId:            v.Vpc.VpcId,
				CidrBlock:        new(fmt.Sprintf("10.94.%d.0/24", i+1)),
				AvailabilityZone: new(region + az),
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, *sn.Subnet.SubnetId)
	}
	g, e := net.CreateSecurityGroup(
		ctx,
		&ec2.CreateSecurityGroupInput{VpcId: v.Vpc.VpcId, GroupName: new("msk-group"), Description: new("metadata")},
	)
	if e != nil {
		t.Fatal(e)
	}
	return &sdk.CreateClusterInput{
		ClusterName: new(
			"records-cluster",
		),
		KafkaVersion:        new("3.9.x"),
		NumberOfBrokerNodes: new(int32(4)),
		EnhancedMonitoring:  types.EnhancedMonitoringPerTopicPerBroker,
		BrokerNodeGroupInfo: &types.BrokerNodeGroupInfo{
			ClientSubnets:  ids,
			InstanceType:   new("kafka.m5.xlarge"),
			SecurityGroups: []string{*g.GroupId},
			StorageInfo:    &types.StorageInfo{EbsStorageInfo: &types.EBSStorageInfo{VolumeSize: new(int32(234))}},
		},
		Tags: map[string]string{"owner": "local"},
	}
}

func mskDescribe(t *testing.T, c *sdk.Client, arn *string) *types.ClusterInfo {
	t.Helper()
	out, e := c.DescribeCluster(context.Background(), &sdk.DescribeClusterInput{ClusterArn: arn})
	if e != nil {
		t.Fatal(e)
	}
	return out.ClusterInfo
}

func mskStatus(t *testing.T, e error, name string, status int) {
	t.Helper()
	code(t, e, name)
	var r *smithyhttp.ResponseError
	if !errors.As(e, &r) || r.HTTPStatusCode() != status {
		t.Fatalf("want %s/%d got %v", name, status, e)
	}
}

func TestMSKSDKLifecycleScopeTagsAndRevision(t *testing.T) {
	s := server(t)
	c := mskClient(s, "test", "us-east-1")
	ctx := context.Background()
	in := mskInput(t, s, "us-east-1")
	out, e := c.CreateCluster(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	b := mskDescribe(t, c, out.ClusterArn)
	if out.State != "" ||
		b.State != "" ||
		b.ActiveOperationArn != nil ||
		b.ZookeeperConnectString != nil ||
		b.ClientAuthentication != nil ||
		b.EncryptionInfo != nil ||
		b.OpenMonitoring != nil ||
		b.LoggingInfo != nil ||
		b.CurrentVersion == nil || b.CreationTime == nil ||
		*b.CurrentBrokerSoftwareInfo.KafkaVersion != "3.9.x" ||
		!reflect.DeepEqual(b.BrokerNodeGroupInfo.ClientSubnets, in.BrokerNodeGroupInfo.ClientSubnets) ||
		*b.BrokerNodeGroupInfo.StorageInfo.EbsStorageInfo.VolumeSize != 234 ||
		b.EnhancedMonitoring != types.EnhancedMonitoringPerTopicPerBroker {
		t.Fatal("retention/runtime boundary", b)
	}
	_, e = c.CreateCluster(ctx, in)
	var duplicate *types.ConflictException
	if !errors.As(e, &duplicate) {
		t.Fatal("typed duplicate", e)
	}
	mskStatus(t, e, "ConflictException", 409)
	update, e := c.UpdateMonitoring(
		ctx,
		&sdk.UpdateMonitoringInput{
			ClusterArn:         out.ClusterArn,
			CurrentVersion:     b.CurrentVersion,
			EnhancedMonitoring: types.EnhancedMonitoringPerBroker,
		},
	)
	if e != nil || update.ClusterOperationArn != nil || *update.ClusterArn != *out.ClusterArn {
		t.Fatal("monitoring", e)
	}
	after := mskDescribe(t, c, out.ClusterArn)
	if *after.CurrentVersion == *b.CurrentVersion || after.EnhancedMonitoring != types.EnhancedMonitoringPerBroker ||
		!after.CreationTime.Equal(*b.CreationTime) {
		t.Fatal("revision")
	}
	_, e = c.UpdateMonitoring(
		ctx,
		&sdk.UpdateMonitoringInput{
			ClusterArn:         out.ClusterArn,
			CurrentVersion:     b.CurrentVersion,
			EnhancedMonitoring: types.EnhancedMonitoringDefault,
		},
	)
	var bad *types.BadRequestException
	if !errors.As(e, &bad) {
		t.Fatal("typed stale", e)
	}
	mskStatus(t, e, "BadRequestException", 400)
	_, e = c.DeleteCluster(ctx, &sdk.DeleteClusterInput{ClusterArn: out.ClusterArn, CurrentVersion: b.CurrentVersion})
	mskStatus(t, e, "BadRequestException", 400)
	if !reflect.DeepEqual(after, mskDescribe(t, c, out.ClusterArn)) {
		t.Fatal("stale wrote")
	}
	_, e = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: out.ClusterArn, Tags: map[string]string{"owner": "merged", "empty": ""}},
	)
	if e != nil {
		t.Fatal(e)
	}
	tags, e := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: out.ClusterArn})
	if e != nil || len(tags.Tags) != 2 || tags.Tags["owner"] != "merged" {
		t.Fatal(tags, e)
	}
	_, e = c.UntagResource(
		ctx,
		&sdk.UntagResourceInput{ResourceArn: out.ClusterArn, TagKeys: []string{"empty", "absent"}},
	)
	if e != nil {
		t.Fatal(e)
	}
	tagger := resourcegroupstaggingapi.New(
		resourcegroupstaggingapi.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	rg, e := tagger.TagResources(
		ctx,
		&resourcegroupstaggingapi.TagResourcesInput{
			ResourceARNList: []string{*out.ClusterArn},
			Tags:            map[string]string{"external": "retained"},
		},
	)
	if e != nil || len(rg.FailedResourcesMap) != 0 {
		t.Fatal(rg, e)
	}
	un, e := tagger.UntagResources(
		ctx,
		&resourcegroupstaggingapi.UntagResourcesInput{
			ResourceARNList: []string{*out.ClusterArn},
			TagKeys:         []string{"external"},
		},
	)
	if e != nil || len(un.FailedResourcesMap) != 0 {
		t.Fatal(un, e)
	}
	for _, other := range []*sdk.Client{mskClient(s, "team-b", "us-east-1"), mskClient(s, "test", "us-west-2")} {
		_, e = other.DescribeCluster(ctx, &sdk.DescribeClusterInput{ClusterArn: out.ClusterArn})
		mskStatus(t, e, "NotFoundException", 404)
		_, e = other.DeleteCluster(ctx, &sdk.DeleteClusterInput{ClusterArn: out.ClusterArn})
		mskStatus(t, e, "NotFoundException", 404)
		_, e = other.TagResource(
			ctx,
			&sdk.TagResourceInput{ResourceArn: out.ClusterArn, Tags: map[string]string{"x": "y"}},
		)
		mskStatus(t, e, "NotFoundException", 404)
		list, err := other.ListClusters(ctx, &sdk.ListClustersInput{})
		if err != nil || len(list.ClusterInfoList) != 0 {
			t.Fatal("scope list", err)
		}
	}
	_, e = mskClient(s, "team-b", "us-east-1").CreateCluster(ctx, in)
	mskStatus(t, e, "BadRequestException", 400)
	list, e := c.ListClusters(
		ctx,
		&sdk.ListClustersInput{ClusterNameFilter: new("records-"), MaxResults: new(int32(1))},
	)
	if e != nil || len(list.ClusterInfoList) != 1 || list.NextToken != nil ||
		*list.ClusterInfoList[0].ClusterArn != *out.ClusterArn {
		t.Fatal("list", e)
	}
	del, e := c.DeleteCluster(
		ctx,
		&sdk.DeleteClusterInput{ClusterArn: out.ClusterArn, CurrentVersion: after.CurrentVersion},
	)
	if e != nil || del.State != "" || *del.ClusterArn != *out.ClusterArn {
		t.Fatal("delete", e)
	}
	_, e = c.DeleteCluster(ctx, &sdk.DeleteClusterInput{ClusterArn: out.ClusterArn})
	var gone *types.NotFoundException
	if !errors.As(e, &gone) {
		t.Fatal("typed notfound", e)
	}
	mskStatus(t, e, "NotFoundException", 404)
}

func mskWire(
	t *testing.T,
	s *httptest.Server,
	method, path, body string,
) (status int, document map[string]any, bodyBytes []byte) {
	t.Helper()
	req, e := http.NewRequest(method, s.URL+path, strings.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(
		"Authorization",
		"AWS4-HMAC-SHA256 Credential=test/20261011/us-east-1/kafka/aws4_request, SignedHeaders=host, Signature=abcd",
	)
	res, e := s.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	raw, e := io.ReadAll(res.Body)
	if e != nil {
		t.Fatal(e)
	}
	var out map[string]any
	if len(raw) > 0 {
		if e = json.Unmarshal(raw, &out); e != nil {
			t.Fatal(string(raw), e)
		}
	}
	return res.StatusCode, out, raw
}

func mskWireInput(in *sdk.CreateClusterInput) map[string]any {
	return map[string]any{
		"clusterName":         *in.ClusterName,
		"kafkaVersion":        *in.KafkaVersion,
		"numberOfBrokerNodes": *in.NumberOfBrokerNodes,
		"enhancedMonitoring":  "PER_TOPIC_PER_BROKER",
		"tags":                in.Tags,
		"brokerNodeGroupInfo": map[string]any{
			"clientSubnets":  in.BrokerNodeGroupInfo.ClientSubnets,
			"instanceType":   *in.BrokerNodeGroupInfo.InstanceType,
			"securityGroups": in.BrokerNodeGroupInfo.SecurityGroups,
			"storageInfo":    map[string]any{"ebsStorageInfo": map[string]any{"volumeSize": 234}},
		},
	}
}

func TestMSKRawPinnedRESTProtocolAndUnsupportedFlags(t *testing.T) {
	s := server(t)
	in := mskInput(t, s, "us-east-1")
	body := mskWireInput(in)
	raw, _ := json.Marshal(body)
	status, out, _ := mskWire(t, s, "POST", "/v1/clusters", string(raw))
	if status != 200 || out["clusterArn"] == nil || out["clusterName"] != *in.ClusterName || out["state"] != nil {
		t.Fatal(status, out)
	}
	arn := out["clusterArn"].(string)
	path := "/v1/clusters/" + url.PathEscape(arn)
	status, out, _ = mskWire(t, s, "GET", path, "")
	if status != 200 {
		t.Fatal(status, out)
	}
	info := out["clusterInfo"].(map[string]any)
	version := info["currentVersion"].(string)
	if info["state"] != nil || info["activeOperationArn"] != nil || info["creationTime"] == nil ||
		info["currentBrokerSoftwareInfo"].(map[string]any)["kafkaVersion"] != "3.9.x" {
		t.Fatal(info)
	}
	// Each excluded nested option fails even when its value looks inactive.
	for _, value := range []any{false, nil, map[string]any{}} {
		for _, member := range []string{
			"loggingInfo",
			"openMonitoring",
			"storageMode",
			"rebalancing",
		} {
			candidate := mskWireInput(in)
			candidate["clusterName"] = "bad-" + member
			candidate[member] = value
			raw, _ = json.Marshal(candidate)
			status, _, _ = mskWire(t, s, "POST", "/v1/clusters", string(raw))
			if status != 501 {
				t.Fatal("excluded create", member, value, status)
			}
		}
		for _, member := range []string{"connectivityInfo", "zoneIds"} {
			candidate := mskWireInput(in)
			candidate["clusterName"] = "bad-nested"
			candidate["brokerNodeGroupInfo"].(map[string]any)[member] = value
			raw, _ = json.Marshal(candidate)
			status, _, _ = mskWire(t, s, "POST", "/v1/clusters", string(raw))
			if status != 501 {
				t.Fatal(member, value, status)
			}
		}
		candidate := mskWireInput(in)
		candidate["clusterName"] = "bad-throughput"
		storage := candidate["brokerNodeGroupInfo"].(map[string]any)["storageInfo"].(map[string]any)
		storage["ebsStorageInfo"].(map[string]any)["provisionedThroughput"] = value
		raw, _ = json.Marshal(candidate)
		status, _, _ = mskWire(t, s, "POST", "/v1/clusters", string(raw))
		if status != 501 {
			t.Fatal("throughput", value, status)
		}
		for _, member := range []string{"loggingInfo", "openMonitoring"} {
			candidate := map[string]any{"currentVersion": version, "enhancedMonitoring": "DEFAULT", member: value}
			raw, _ = json.Marshal(candidate)
			status, _, _ = mskWire(t, s, "PUT", path+"/monitoring", string(raw))
			if status != 501 {
				t.Fatal("excluded update", member, value, status)
			}
		}
	}
	for _, body := range []string{`{}`, `{"brokerNodeGroupInfo":null}`, `{"ClusterName":"wrong-case"}`} {
		status, _, _ = mskWire(t, s, "POST", "/v1/clusters", body)
		if status < 400 {
			t.Fatal("required/casing", status)
		}
	}
	status, out, _ = mskWire(t, s, "GET", "/v1/clusters?clusterNameFilter=records-&maxResults=1", "")
	if status != 200 || len(out["clusterInfoList"].([]any)) != 1 {
		t.Fatal("query", status, out)
	}
	for _, query := range []string{
		"maxResults=0",
		"maxResults=101",
		"maxResults=bad",
		"nextToken=bad",
		"unknown=false",
		"MaxResults=1",
	} {
		status, _, _ = mskWire(t, s, "GET", "/v1/clusters?"+query, "")
		if status < 400 {
			t.Fatal("invalid query", query, status)
		}
	}
	status, out, _ = mskWire(
		t,
		s,
		"PUT",
		path+"/monitoring",
		fmt.Sprintf(`{"currentVersion":%q,"enhancedMonitoring":"DEFAULT"}`, version),
	)
	if status != 200 || out["clusterArn"] != arn || out["clusterOperationArn"] != nil {
		t.Fatal("monitoring binding", status, out)
	}
	status, out, _ = mskWire(t, s, "GET", path, "")
	info = out["clusterInfo"].(map[string]any)
	updatedVersion := info["currentVersion"].(string)
	if status != 200 || updatedVersion == version || info["enhancedMonitoring"] != "DEFAULT" {
		t.Fatal("update retention")
	}
	tagsPath := "/v1/tags/" + url.PathEscape(arn)
	status, _, raw = mskWire(t, s, "POST", tagsPath, `{"tags":{"a":"","b":"keep"}}`)
	if status != 204 || len(raw) != 0 {
		t.Fatal("unit output", status, string(raw))
	}
	status, out, _ = mskWire(t, s, "GET", tagsPath, "")
	if status != 200 || len(out["tags"].(map[string]any)) != 3 {
		t.Fatal("tags", status, out)
	}
	status, _, raw = mskWire(t, s, "DELETE", tagsPath+"?tagKeys=a&tagKeys=absent", "")
	if status != 204 || len(raw) != 0 {
		t.Fatal("tag keys binding")
	}
	for _, route := range []struct{ verb, path string }{
		{"GET", "/v2/clusters"},
		{"POST", "/v2/clusters"},
		{"POST", "/v1/configurations"},
		{"PUT", path + "/security"},
		{"GET", path + "/scram-secrets"},
		{"GET", path + "/operations"},
	} {
		status, _, _ = mskWire(t, s, route.verb, route.path, "")
		if status >= 200 && status < 300 {
			t.Fatal("runtime/unsupported route succeeded", route)
		}
	}
	status, out, _ = mskWire(t, s, "DELETE", path+"?currentVersion="+version, "")
	if status != 400 || out["message"] == nil {
		t.Fatal("stale delete", status, out)
	}
	status, out, _ = mskWire(t, s, "DELETE", path+"?currentVersion="+updatedVersion, "")
	if status != 200 || out["clusterArn"] != arn {
		t.Fatal("delete binding", status, out)
	}
	status, out, _ = mskWire(t, s, "GET", path, "")
	if status != 404 || out["message"] == nil || !strings.Contains(out["__type"].(string), "NotFoundException") {
		t.Fatal("modeled error", status, out)
	}
}

func TestMSKVerifyAllSelectedSignatures(t *testing.T) {
	s := verifyingServer(t)
	c := mskClient(s, "test", "us-east-1")
	ctx := context.Background()
	created, e := c.CreateCluster(ctx, mskInput(t, s, "us-east-1"))
	if e != nil {
		t.Fatal(e)
	}
	b := mskDescribe(t, c, created.ClusterArn)
	if _, e = c.ListClusters(ctx, &sdk.ListClustersInput{MaxResults: new(int32(1))}); e != nil {
		t.Fatal(e)
	}
	if _, e = c.UpdateMonitoring(
		ctx,
		&sdk.UpdateMonitoringInput{
			ClusterArn:         created.ClusterArn,
			CurrentVersion:     b.CurrentVersion,
			EnhancedMonitoring: types.EnhancedMonitoringDefault,
		},
	); e != nil {
		t.Fatal(e)
	}
	if _, e = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: created.ClusterArn, Tags: map[string]string{"signed": "yes"}},
	); e != nil {
		t.Fatal(e)
	}
	if _, e = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: created.ClusterArn}); e != nil {
		t.Fatal(e)
	}
	if _, e = c.UntagResource(
		ctx,
		&sdk.UntagResourceInput{ResourceArn: created.ClusterArn, TagKeys: []string{"signed"}},
	); e != nil {
		t.Fatal(e)
	}
	_, e = mskClient(s, "wrong", "us-east-1").ListClusters(ctx, &sdk.ListClustersInput{})
	code(t, e, "InvalidSignatureException")
	if _, e = c.DeleteCluster(
		ctx,
		&sdk.DeleteClusterInput{
			ClusterArn:     created.ClusterArn,
			CurrentVersion: mskDescribe(t, c, created.ClusterArn).CurrentVersion,
		},
	); e != nil {
		t.Fatal(e)
	}
}

func TestMSKSQLiteOfflineFixtureRestartAndReset(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := mskClient(s, "test", "us-west-2")
	in := mskInput(t, s, "us-west-2")
	out, e := c.CreateCluster(ctx, in)
	if e != nil {
		stop()
		t.Fatal(e)
	}
	b := mskDescribe(t, c, out.ClusterArn)
	if _, e = c.UpdateMonitoring(
		ctx,
		&sdk.UpdateMonitoringInput{
			ClusterArn:         out.ClusterArn,
			CurrentVersion:     b.CurrentVersion,
			EnhancedMonitoring: types.EnhancedMonitoringPerTopicPerPartition,
		},
	); e != nil {
		stop()
		t.Fatal(e)
	}
	if _, e = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: out.ClusterArn, Tags: map[string]string{"snapshot": "exact"}},
	); e != nil {
		stop()
		t.Fatal(e)
	}
	before := mskDescribe(t, c, out.ClusterArn)
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	c = mskClient(s, "test", "us-west-2")
	if !reflect.DeepEqual(before, mskDescribe(t, c, out.ClusterArn)) {
		stop()
		t.Fatal("SQLite exact comparison")
	}
	stop()
	fixture := filepath.Join(t.TempDir(), "msk.tar.gz")
	if e = archive.Export(ctx, archive.ExportOptions{StorageDir: dir, OutputFile: fixture}); e != nil {
		t.Fatal(e)
	}
	child := filepath.Join(t.TempDir(), "child")
	if e = archive.Import(ctx, archive.ImportOptions{StorageDir: child, InputFile: fixture}); e != nil {
		t.Fatal(e)
	}
	s, stop = persistent(t, child, &fakeRuntime{})
	c = mskClient(s, "test", "us-west-2")
	if !reflect.DeepEqual(before, mskDescribe(t, c, out.ClusterArn)) {
		stop()
		t.Fatal("offline fixture metadata")
	}
	stop()
	s, stop = persistent(t, child, &fakeRuntime{})
	defer stop()
	c = mskClient(s, "test", "us-west-2")
	if !reflect.DeepEqual(before, mskDescribe(t, c, out.ClusterArn)) {
		t.Fatal("imported normal restart")
	}
	res, e := s.Client().Get(s.URL + "/_tinystack/resources")
	if e != nil {
		t.Fatal(e)
	}
	raw, e := io.ReadAll(res.Body)
	res.Body.Close()
	if e != nil || !strings.Contains(string(raw), *out.ClusterArn) || !strings.Contains(string(raw), "RecordsOnly") ||
		!strings.Contains(string(raw), "ExecutionReady") {
		t.Fatal("inventory", e)
	}
	status, _ := post(t, s, "/_tinystack/reset", nil)
	if status != 200 {
		t.Fatal("reset")
	}
	list, e := c.ListClusters(ctx, &sdk.ListClustersInput{})
	if e != nil || len(list.ClusterInfoList) != 0 {
		t.Fatal("reset records", e)
	}
	res, e = s.Client().Get(s.URL + "/_tinystack/resources")
	if e != nil {
		t.Fatal(e)
	}
	raw, e = io.ReadAll(res.Body)
	res.Body.Close()
	if e != nil || strings.Contains(string(raw), *out.ClusterArn) {
		t.Fatal("reset inventory", e)
	}
}

func TestMSKSDKPaginationDeletedCursorAndFilterTokens(t *testing.T) {
	s := server(t)
	c := mskClient(s, "test", "us-east-1")
	in := mskInput(t, s, "us-east-1")
	ctx := context.Background()
	for i := range 7 {
		in.ClusterName = new(fmt.Sprintf("page-%02d", i))
		if _, e := c.CreateCluster(ctx, in); e != nil {
			t.Fatal(e)
		}
	}
	first, e := c.ListClusters(ctx, &sdk.ListClustersInput{ClusterNameFilter: new("page-"), MaxResults: new(int32(2))})
	if e != nil || len(first.ClusterInfoList) != 2 || first.NextToken == nil {
		t.Fatal(e)
	}
	if _, e = c.DeleteCluster(ctx, &sdk.DeleteClusterInput{ClusterArn: first.ClusterInfoList[1].ClusterArn}); e != nil {
		t.Fatal(e)
	}
	second, e := c.ListClusters(
		ctx,
		&sdk.ListClustersInput{ClusterNameFilter: new("page-"), MaxResults: new(int32(2)), NextToken: first.NextToken},
	)
	if e != nil || *second.ClusterInfoList[0].ClusterName != "page-02" || len(second.ClusterInfoList) != 2 ||
		second.NextToken == nil {
		t.Fatal("resume", e)
	}
	last, e := c.ListClusters(ctx, &sdk.ListClustersInput{ClusterNameFilter: new("page-"), NextToken: second.NextToken})
	if e != nil || len(last.ClusterInfoList) != 3 || last.NextToken != nil {
		t.Fatal("terminal", e)
	}
	_, e = c.ListClusters(ctx, &sdk.ListClustersInput{NextToken: first.NextToken})
	mskStatus(t, e, "BadRequestException", 400)
	for _, other := range []*sdk.Client{mskClient(s, "team-b", "us-east-1"), mskClient(s, "test", "us-west-2")} {
		_, e = other.ListClusters(
			ctx,
			&sdk.ListClustersInput{ClusterNameFilter: new("page-"), NextToken: first.NextToken},
		)
		mskStatus(t, e, "BadRequestException", 400)
	}
}

func TestMSKSDKScopedRealEC2ReferenceValidationAndDefaults(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := mskClient(s, "test", "us-east-1")
	in := mskInput(t, s, "us-east-1")
	net := ec2Client(s, "test", "us-east-1")
	foreign := mskInput(t, s, "us-east-1")
	subnets := in.BrokerNodeGroupInfo.ClientSubnets
	described, e := net.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: []string{subnets[0]}})
	if e != nil {
		t.Fatal(e)
	}
	excluded, e := net.CreateSubnet(
		ctx,
		&ec2.CreateSubnetInput{
			VpcId:            described.Subnets[0].VpcId,
			CidrBlock:        new("10.94.3.0/24"),
			AvailabilityZone: new("us-east-1c"),
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	in.EnhancedMonitoring = ""
	for label, ids := range map[string][]string{
		"duplicate":     {subnets[0], subnets[0]},
		"cross-vpc":     {subnets[0], foreign.BrokerNodeGroupInfo.ClientSubnets[1]},
		"excluded-zone": {subnets[0], *excluded.Subnet.SubnetId},
		"missing":       {subnets[0], "subnet-00000000"},
	} {
		candidate := *in
		group := *in.BrokerNodeGroupInfo
		candidate.BrokerNodeGroupInfo = &group
		candidate.ClusterName = new(label)
		group.ClientSubnets = ids
		_, e = c.CreateCluster(ctx, &candidate)
		mskStatus(t, e, "BadRequestException", 400)
	}
	candidate := *in
	group := *in.BrokerNodeGroupInfo
	candidate.BrokerNodeGroupInfo = &group
	group.SecurityGroups = foreign.BrokerNodeGroupInfo.SecurityGroups
	_, e = c.CreateCluster(ctx, &candidate)
	mskStatus(t, e, "BadRequestException", 400)
	list, e := c.ListClusters(ctx, &sdk.ListClustersInput{})
	if e != nil || len(list.ClusterInfoList) != 0 {
		t.Fatal("invalid EC2 references persisted")
	}
	out, e := c.CreateCluster(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	b := mskDescribe(t, c, out.ClusterArn)
	if b.EnhancedMonitoring != types.EnhancedMonitoringDefault ||
		b.BrokerNodeGroupInfo.BrokerAZDistribution != types.BrokerAZDistributionDefault {
		t.Fatal("assembled defaults")
	}
	other := mskClient(s, "team-b", "us-east-1")
	_, e = other.UpdateMonitoring(
		ctx,
		&sdk.UpdateMonitoringInput{
			ClusterArn:         out.ClusterArn,
			CurrentVersion:     b.CurrentVersion,
			EnhancedMonitoring: types.EnhancedMonitoringPerBroker,
		},
	)
	mskStatus(t, e, "NotFoundException", 404)
	_, e = other.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: out.ClusterArn})
	mskStatus(t, e, "NotFoundException", 404)
	// References are configuration records: deleting an EC2 subnet after creation
	// does not erase or rewrite the captured cluster config during monitoring updates.
	if _, e = net.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: new(subnets[0])}); e != nil {
		t.Fatal(e)
	}
	if _, e = c.UpdateMonitoring(
		ctx,
		&sdk.UpdateMonitoringInput{
			ClusterArn:         out.ClusterArn,
			CurrentVersion:     b.CurrentVersion,
			EnhancedMonitoring: types.EnhancedMonitoringPerBroker,
		},
	); e != nil {
		t.Fatal(e)
	}
	after := mskDescribe(t, c, out.ClusterArn)
	if !reflect.DeepEqual(b.BrokerNodeGroupInfo, after.BrokerNodeGroupInfo) {
		t.Fatal("update rewrote retained network")
	}
}

func TestMSKConfigurationsAndClusterUpdates(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := mskClient(s, "test", "us-east-1")
	config, err := c.CreateConfiguration(
		ctx,
		&sdk.CreateConfigurationInput{
			Name:             new("custom"),
			KafkaVersions:    []string{"3.8.x", "3.9.x"},
			ServerProperties: []byte("auto.create.topics.enable=true\n"),
			Description:      new("first"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateConfiguration(
		ctx,
		&sdk.CreateConfigurationInput{Name: new("custom"), ServerProperties: []byte("a=b")},
	)
	code(t, err, "ConflictException")
	_, err = c.CreateConfiguration(
		ctx,
		&sdk.CreateConfigurationInput{Name: new("bad"), ServerProperties: []byte("invalid")},
	)
	code(t, err, "BadRequestException")
	_, err = c.CreateConfiguration(ctx, &sdk.CreateConfigurationInput{
		Name: new("bad_name"), ServerProperties: []byte("auto.create.topics.enable=true"),
	})
	code(t, err, "BadRequestException")
	list, err := c.ListConfigurations(ctx, &sdk.ListConfigurationsInput{MaxResults: new(int32(1))})
	if err != nil || len(list.Configurations) != 1 {
		t.Fatal(list, err)
	}
	described, err := c.DescribeConfiguration(ctx, &sdk.DescribeConfigurationInput{Arn: config.Arn})
	if err != nil || *described.Name != "custom" {
		t.Fatal(described, err)
	}
	_, err = mskClient(
		s,
		"test",
		"us-west-2",
	).DescribeConfiguration(ctx, &sdk.DescribeConfigurationInput{Arn: config.Arn})
	code(t, err, "NotFoundException")
	updated, err := c.UpdateConfiguration(
		ctx,
		&sdk.UpdateConfigurationInput{
			Arn:              config.Arn,
			ServerProperties: []byte("auto.create.topics.enable=false\n"),
			Description:      new("second"),
		},
	)
	if err != nil || *updated.LatestRevision.Revision != 2 {
		t.Fatal(updated, err)
	}
	revisions, err := c.ListConfigurationRevisions(
		ctx,
		&sdk.ListConfigurationRevisionsInput{Arn: config.Arn, MaxResults: new(int32(1))},
	)
	if err != nil || len(revisions.Revisions) != 1 || revisions.NextToken == nil {
		t.Fatal(revisions, err)
	}
	next, err := c.ListConfigurationRevisions(
		ctx,
		&sdk.ListConfigurationRevisionsInput{
			Arn:        config.Arn,
			MaxResults: new(int32(1)),
			NextToken:  revisions.NextToken,
		},
	)
	if err != nil || *next.Revisions[0].Revision != 2 {
		t.Fatal(next, err)
	}
	rev, err := c.DescribeConfigurationRevision(
		ctx,
		&sdk.DescribeConfigurationRevisionInput{Arn: config.Arn, Revision: new(int64(1))},
	)
	if err != nil || string(rev.ServerProperties) != "auto.create.topics.enable=true\n" {
		t.Fatal(rev, err)
	}
	_, err = c.DescribeConfigurationRevision(
		ctx,
		&sdk.DescribeConfigurationRevisionInput{Arn: config.Arn, Revision: new(int64(3))},
	)
	code(t, err, "NotFoundException")
	in := mskInput(t, s, "us-east-1")
	in.KafkaVersion = new("3.8.x")
	in.ConfigurationInfo = &types.ConfigurationInfo{Arn: config.Arn, Revision: new(int64(1))}
	created, err := c.CreateCluster(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	arn := created.ClusterArn
	r := mskDescribe(t, c, arn)
	if *r.CurrentBrokerSoftwareInfo.ConfigurationRevision != 1 {
		t.Fatal(r)
	}
	_, err = c.DeleteConfiguration(ctx, &sdk.DeleteConfigurationInput{Arn: config.Arn})
	code(t, err, "BadRequestException")
	_, err = c.UpdateBrokerCount(
		ctx,
		&sdk.UpdateBrokerCountInput{
			ClusterArn:                arn,
			CurrentVersion:            new("stale"),
			TargetNumberOfBrokerNodes: new(int32(6)),
		},
	)
	code(t, err, "BadRequestException")
	for _, n := range []int32{2, 4, 5} {
		_, err = c.UpdateBrokerCount(
			ctx,
			&sdk.UpdateBrokerCountInput{
				ClusterArn:                arn,
				CurrentVersion:            r.CurrentVersion,
				TargetNumberOfBrokerNodes: new(n),
			},
		)
		code(t, err, "BadRequestException")
	}
	count, err := c.UpdateBrokerCount(
		ctx,
		&sdk.UpdateBrokerCountInput{
			ClusterArn:                arn,
			CurrentVersion:            r.CurrentVersion,
			TargetNumberOfBrokerNodes: new(int32(6)),
		},
	)
	if err != nil || count.ClusterOperationArn == nil {
		t.Fatal(count, err)
	}
	if mskDescribe(t, c, arn).State != types.ClusterStateUpdating {
		t.Fatal("missing UPDATING")
	}
	pending := mskDescribe(t, c, arn)
	_, err = c.UpdateBrokerCount(ctx, &sdk.UpdateBrokerCountInput{
		ClusterArn: arn, CurrentVersion: pending.CurrentVersion, TargetNumberOfBrokerNodes: new(int32(8)),
	})
	code(t, err, "BadRequestException")

	wait := func() *types.ClusterInfo {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			r = mskDescribe(t, c, arn)
			if r.State == types.ClusterStateActive {
				return r
			}
			if time.Now().After(deadline) {
				t.Fatal("transition timeout")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	r = wait()
	if *r.NumberOfBrokerNodes != 6 {
		t.Fatal(r)
	}
	for _, b := range []types.BrokerEBSVolumeInfo{
		{KafkaBrokerNodeId: new("1"), VolumeSizeGB: new(int32(300))},
		{KafkaBrokerNodeId: new("ALL"), VolumeSizeGB: new(int32(200))},
	} {
		_, err = c.UpdateBrokerStorage(
			ctx,
			&sdk.UpdateBrokerStorageInput{
				ClusterArn:                arn,
				CurrentVersion:            r.CurrentVersion,
				TargetBrokerEBSVolumeInfo: []types.BrokerEBSVolumeInfo{b},
			},
		)
		code(t, err, "BadRequestException")
	}
	_, err = c.UpdateBrokerStorage(
		ctx,
		&sdk.UpdateBrokerStorageInput{
			ClusterArn:     arn,
			CurrentVersion: r.CurrentVersion,
			TargetBrokerEBSVolumeInfo: []types.BrokerEBSVolumeInfo{
				{KafkaBrokerNodeId: new("ALL"), VolumeSizeGB: new(int32(300))},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	r = wait()
	if *r.BrokerNodeGroupInfo.StorageInfo.EbsStorageInfo.VolumeSize != 300 {
		t.Fatal(r)
	}
	_, err = c.UpdateClusterConfiguration(
		ctx,
		&sdk.UpdateClusterConfigurationInput{
			ClusterArn:        arn,
			CurrentVersion:    r.CurrentVersion,
			ConfigurationInfo: &types.ConfigurationInfo{Arn: config.Arn, Revision: new(int64(3))},
		},
	)
	code(t, err, "BadRequestException")
	_, err = c.UpdateClusterConfiguration(
		ctx,
		&sdk.UpdateClusterConfigurationInput{
			ClusterArn:        arn,
			CurrentVersion:    r.CurrentVersion,
			ConfigurationInfo: &types.ConfigurationInfo{Arn: config.Arn, Revision: new(int64(2))},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	r = wait()
	if *r.CurrentBrokerSoftwareInfo.ConfigurationRevision != 2 {
		t.Fatal(r)
	}
	_, err = c.UpdateClusterKafkaVersion(
		ctx,
		&sdk.UpdateClusterKafkaVersionInput{
			ClusterArn:         arn,
			CurrentVersion:     r.CurrentVersion,
			TargetKafkaVersion: new("3.9.x"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	r = wait()
	if *r.CurrentBrokerSoftwareInfo.KafkaVersion != "3.9.x" {
		t.Fatal(r)
	}
	v2, err := c.DescribeClusterV2(ctx, &sdk.DescribeClusterV2Input{ClusterArn: arn})
	if err != nil || v2.ClusterInfo.ClusterType != types.ClusterTypeProvisioned ||
		*v2.ClusterInfo.Provisioned.NumberOfBrokerNodes != 6 {
		t.Fatal(v2, err)
	}
	clusters, err := c.ListClustersV2(ctx, &sdk.ListClustersV2Input{ClusterTypeFilter: new("PROVISIONED")})
	if err != nil || len(clusters.ClusterInfoList) != 1 {
		t.Fatal(clusters, err)
	}
	empty, err := c.ListClustersV2(ctx, &sdk.ListClustersV2Input{ClusterTypeFilter: new("SERVERLESS")})
	if err != nil || len(empty.ClusterInfoList) != 0 {
		t.Fatal(empty, err)
	}
	_, err = c.DeleteCluster(ctx, &sdk.DeleteClusterInput{ClusterArn: arn, CurrentVersion: r.CurrentVersion})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DeleteConfiguration(ctx, &sdk.DeleteConfigurationInput{Arn: config.Arn})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DescribeConfiguration(ctx, &sdk.DescribeConfigurationInput{Arn: config.Arn})
	code(t, err, "NotFoundException")
}

func TestMSKConfigurationCompatibilityAndPagination(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := mskClient(s, "test", "us-east-1")
	var configuration *string
	for _, name := range []string{"a", "b"} {
		out, err := c.CreateConfiguration(ctx, &sdk.CreateConfigurationInput{
			Name: new(
				name,
			),
			KafkaVersions:    []string{"3.8.x"},
			ServerProperties: []byte("auto.create.topics.enable=true"),
		})
		if err != nil {
			t.Fatal(err)
		}
		configuration = out.Arn
	}
	page, err := c.ListConfigurations(ctx, &sdk.ListConfigurationsInput{MaxResults: new(int32(1))})
	if err != nil || page.NextToken == nil {
		t.Fatal(page, err)
	}
	rest, err := c.ListConfigurations(
		ctx,
		&sdk.ListConfigurationsInput{MaxResults: new(int32(1)), NextToken: page.NextToken},
	)
	if err != nil || len(rest.Configurations) != 1 || *rest.Configurations[0].Name != "b" {
		t.Fatal(rest, err)
	}
	_, err = mskClient(
		s,
		"test",
		"us-west-2",
	).ListConfigurations(ctx, &sdk.ListConfigurationsInput{NextToken: page.NextToken})
	code(t, err, "BadRequestException")
	in := mskInput(t, s, "us-east-1")
	in.ConfigurationInfo = &types.ConfigurationInfo{Arn: configuration, Revision: new(int64(1))}
	_, err = c.CreateCluster(ctx, in)
	code(t, err, "BadRequestException")
	in.KafkaVersion = new("3.8.x")
	created, err := c.CreateCluster(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	r := mskDescribe(t, c, created.ClusterArn)
	_, err = c.UpdateClusterKafkaVersion(ctx, &sdk.UpdateClusterKafkaVersionInput{
		ClusterArn: created.ClusterArn, CurrentVersion: r.CurrentVersion, TargetKafkaVersion: new("3.9.x"),
	})
	code(t, err, "BadRequestException")
	if *mskDescribe(t, c, created.ClusterArn).CurrentVersion != *r.CurrentVersion {
		t.Fatal("failed upgrade changed cluster")
	}
	in.ClusterName = new("second")
	in.ConfigurationInfo = nil
	_, err = c.CreateCluster(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := c.ListClustersV2(ctx, &sdk.ListClustersV2Input{MaxResults: new(int32(1))})
	if err != nil || v2.NextToken == nil {
		t.Fatal(v2, err)
	}
	_, err = c.ListClusters(ctx, &sdk.ListClustersInput{NextToken: v2.NextToken})
	code(t, err, "BadRequestException")
	_, err = c.ListClustersV2(
		ctx,
		&sdk.ListClustersV2Input{NextToken: v2.NextToken, ClusterTypeFilter: new("PROVISIONED")},
	)
	code(t, err, "BadRequestException")
	next, err := c.ListClustersV2(ctx, &sdk.ListClustersV2Input{NextToken: v2.NextToken})
	if err != nil || len(next.ClusterInfoList) != 1 {
		t.Fatal(next, err)
	}
}
