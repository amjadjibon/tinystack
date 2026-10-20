package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/databasemigrationservice"
	"github.com/aws/aws-sdk-go-v2/service/databasemigrationservice/types"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/smithy-go"
)

const dmsMappings = `{"rules":[{
 "rule-type":"selection","rule-id":"1","rule-name":"all",
 "object-locator":{"schema-name":"%","table-name":"%"},"rule-action":"include"
}]}`

func dmsClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func wantDMSError(t *testing.T, err error, code string) {
	t.Helper()
	var e smithy.APIError
	if !errors.As(err, &e) || e.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func dmsNetwork(t *testing.T, s *httptest.Server) (subnets []string, groupID string) {
	t.Helper()
	ctx := context.Background()
	c := ec2Client(s, "test", "us-east-1")
	v, err := c.CreateVpc(ctx, &ec2sdk.CreateVpcInput{CidrBlock: new("10.35.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}

	ids := []string{}
	for i, zone := range []string{"us-east-1a", "us-east-1b", "us-east-1a"} {
		sn, e := c.CreateSubnet(ctx, &ec2sdk.CreateSubnetInput{
			VpcId:     v.Vpc.VpcId,
			CidrBlock: new(fmt.Sprintf("10.35.%d.0/24", i)), AvailabilityZone: new(zone),
		})
		if e != nil {
			t.Fatal(e)
		}

		ids = append(ids, *sn.Subnet.SubnetId)
	}

	g, err := c.CreateSecurityGroup(ctx, &ec2sdk.CreateSecurityGroupInput{
		VpcId:     v.Vpc.VpcId,
		GroupName: new("dms"), Description: new("DMS metadata"),
	})
	if err != nil {
		t.Fatal(err)
	}

	return ids, *g.GroupId
}

func TestDMSSDKControlPlane(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := dmsClient(s, "test", "us-east-1")
	subnets, group := dmsNetwork(t, s)
	subnetIn := &sdk.CreateReplicationSubnetGroupInput{
		ReplicationSubnetGroupIdentifier:  new("migrations"),
		ReplicationSubnetGroupDescription: new("local"), SubnetIds: subnets[:2],
		Tags: []types.Tag{{Key: new("team"), Value: new("core")}},
	}
	sn, err := c.CreateReplicationSubnetGroup(ctx, subnetIn)
	if err != nil || len(sn.ReplicationSubnetGroup.Subnets) != 2 {
		t.Fatal(sn, err)
	}

	_, err = c.CreateReplicationSubnetGroup(ctx, subnetIn)
	wantDMSError(t, err, "ResourceAlreadyExistsFault")
	_, err = c.ModifyReplicationSubnetGroup(ctx, &sdk.ModifyReplicationSubnetGroupInput{
		ReplicationSubnetGroupIdentifier:  subnetIn.ReplicationSubnetGroupIdentifier,
		ReplicationSubnetGroupDescription: new("updated"), SubnetIds: subnets,
	})
	if err != nil {
		t.Fatal(err)
	}

	instance, err := c.CreateReplicationInstance(ctx, &sdk.CreateReplicationInstanceInput{
		ReplicationInstanceIdentifier:    new("worker"),
		ReplicationInstanceClass:         new("dms.t3.micro"),
		ReplicationSubnetGroupIdentifier: subnetIn.ReplicationSubnetGroupIdentifier,
		VpcSecurityGroupIds: []string{
			group,
		},
		EngineVersion:      new("3.5.3"),
		AllocatedStorage:   new(int32(50)),
		MultiAZ:            new(true),
		PubliclyAccessible: new(false),
	})
	if err != nil || aws.ToString(instance.ReplicationInstance.ReplicationInstanceStatus) != "creating" {
		t.Fatal(instance, err)
	}

	arn := instance.ReplicationInstance.ReplicationInstanceArn
	for _, state := range []string{"creating", "available"} {
		got, e := c.DescribeReplicationInstances(ctx, &sdk.DescribeReplicationInstancesInput{Filters: []types.Filter{
			{Name: new("replication-instance-arn"), Values: []string{*arn}},
		}})
		if e != nil || len(got.ReplicationInstances) != 1 ||
			aws.ToString(got.ReplicationInstances[0].ReplicationInstanceStatus) != state {
			t.Fatal(got, e)
		}
	}

	modified, err := c.ModifyReplicationInstance(ctx, &sdk.ModifyReplicationInstanceInput{
		ReplicationInstanceArn:        arn,
		ReplicationInstanceIdentifier: new("renamed-worker"), AllocatedStorage: new(int32(100)), ApplyImmediately: true,
	})
	if err != nil || modified.ReplicationInstance.AllocatedStorage != 100 ||
		aws.ToString(modified.ReplicationInstance.ReplicationInstanceArn) != *arn {
		t.Fatal(modified, err)
	}

	_, err = c.RebootReplicationInstance(ctx, &sdk.RebootReplicationInstanceInput{
		ReplicationInstanceArn: arn, ForcePlannedFailover: new(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	for range 2 {
		_, err = c.DescribeReplicationInstances(ctx, &sdk.DescribeReplicationInstancesInput{})
		if err != nil {
			t.Fatal(err)
		}
	}

	source, err := c.CreateEndpoint(ctx, &sdk.CreateEndpointInput{
		EndpointIdentifier: new("source"),
		EndpointType:       types.ReplicationEndpointTypeValueSource, EngineName: new("postgres"),
		ServerName: new("local-db"), Port: new(int32(5432)), Username: new("admin"), Password: new("dms-secret-canary"),
		PostgreSQLSettings: &types.PostgreSQLSettings{HeartbeatEnable: new(true), Password: new("dms-secret-nested")},
	})
	if err != nil {
		t.Fatal(err)
	}

	target, err := c.CreateEndpoint(ctx, &sdk.CreateEndpointInput{
		EndpointIdentifier: new("target"),
		EndpointType:       types.ReplicationEndpointTypeValueTarget, EngineName: new("s3"),
		S3Settings: &types.S3Settings{BucketName: new("records-only"), BucketFolder: new("data")},
	})
	if err != nil || aws.ToString(target.Endpoint.S3Settings.BucketName) != "records-only" {
		t.Fatal(target, err)
	}

	changed, err := c.ModifyEndpoint(ctx, &sdk.ModifyEndpointInput{
		EndpointArn:  source.Endpoint.EndpointArn,
		DatabaseName: new("app"), Password: new("dms-secret-replacement"),
	})
	if err != nil || aws.ToString(changed.Endpoint.DatabaseName) != "app" ||
		changed.Endpoint.PostgreSQLSettings == nil || changed.Endpoint.PostgreSQLSettings.Password != nil {
		t.Fatal(changed, err)
	}

	taskIn := &sdk.CreateReplicationTaskInput{
		ReplicationTaskIdentifier: new("copy"),
		ReplicationInstanceArn:    arn,
		SourceEndpointArn:         source.Endpoint.EndpointArn,
		TargetEndpointArn:         target.Endpoint.EndpointArn,
		MigrationType:             types.MigrationTypeValueFullLoadAndCdc,
		TableMappings:             new(dmsMappings),
		ReplicationTaskSettings:   new(`{"Logging":{"EnableLogging":false}}`),
	}
	for _, mappings := range []string{
		`null`, `[]`, `{}`, `{"rules":[]}`, `{"rules":[{}]}`,
		`{"rules":[{"rule-type":"selection","rule-id":"1","rule-name":"bad","rule-action":"include"}]}`,
	} {
		bad := *taskIn
		bad.TableMappings = new(mappings)
		_, err = c.CreateReplicationTask(ctx, &bad)
		wantDMSError(t, err, "InvalidParameterValueException")
	}

	bad := *taskIn
	bad.MigrationType = types.MigrationTypeValue("invalid")
	_, err = c.CreateReplicationTask(ctx, &bad)
	wantDMSError(t, err, "InvalidParameterValueException")
	bad = *taskIn
	bad.SourceEndpointArn = target.Endpoint.EndpointArn
	_, err = c.CreateReplicationTask(ctx, &bad)
	wantDMSError(t, err, "InvalidParameterValueException")
	task, err := c.CreateReplicationTask(ctx, taskIn)
	if err != nil {
		t.Fatal(err)
	}

	taskArn := task.ReplicationTask.ReplicationTaskArn
	describe := func(state string) {
		t.Helper()
		got, e := c.DescribeReplicationTasks(ctx, &sdk.DescribeReplicationTasksInput{Filters: []types.Filter{
			{Name: new("replication-task-arn"), Values: []string{*taskArn}},
		}})
		if e != nil || len(got.ReplicationTasks) != 1 || aws.ToString(got.ReplicationTasks[0].Status) != state {
			t.Fatalf("want %s: %v %v", state, got, e)
		}
	}
	describe("creating")
	describe("ready")
	_, err = c.ModifyReplicationTask(ctx, &sdk.ModifyReplicationTaskInput{
		ReplicationTaskArn:        taskArn,
		ReplicationTaskIdentifier: new("renamed-copy"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.StartReplicationTask(ctx, &sdk.StartReplicationTaskInput{
		ReplicationTaskArn:       taskArn,
		StartReplicationTaskType: types.StartReplicationTaskTypeValueStartReplication,
	})
	if err != nil {
		t.Fatal(err)
	}

	describe("starting")
	describe("running")
	_, err = c.DeleteReplicationTask(ctx, &sdk.DeleteReplicationTaskInput{ReplicationTaskArn: taskArn})
	wantDMSError(t, err, "InvalidResourceStateFault")
	_, err = c.DeleteEndpoint(ctx, &sdk.DeleteEndpointInput{EndpointArn: source.Endpoint.EndpointArn})
	wantDMSError(t, err, "InvalidResourceStateFault")
	_, err = c.StopReplicationTask(ctx, &sdk.StopReplicationTaskInput{ReplicationTaskArn: taskArn})
	if err != nil {
		t.Fatal(err)
	}

	describe("stopped")
	_, err = c.ModifyReplicationTask(ctx, &sdk.ModifyReplicationTaskInput{
		ReplicationTaskArn: taskArn,
		MigrationType:      types.MigrationTypeValueFullLoad,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.StartReplicationTask(ctx, &sdk.StartReplicationTaskInput{
		ReplicationTaskArn:       taskArn,
		StartReplicationTaskType: types.StartReplicationTaskTypeValueReloadTarget,
	})
	if err != nil {
		t.Fatal(err)
	}

	describe("starting")
	describe("running")
	describe("stopped")

	for _, resource := range []*string{source.Endpoint.EndpointArn, target.Endpoint.EndpointArn, arn, taskArn} {
		_, err = c.AddTagsToResource(ctx, &sdk.AddTagsToResourceInput{
			ResourceArn: resource,
			Tags:        []types.Tag{{Key: new("owner"), Value: new("local")}},
		})
		if err != nil {
			t.Fatal(err)
		}

		tags, e := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: resource})
		if e != nil || len(tags.TagList) != 1 || aws.ToString(tags.TagList[0].Value) != "local" {
			t.Fatal(tags, e)
		}

		_, e = c.RemoveTagsFromResource(
			ctx,
			&sdk.RemoveTagsFromResourceInput{ResourceArn: resource, TagKeys: []string{"owner"}},
		)
		if e != nil {
			t.Fatal(e)
		}
	}

	taggingClient := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := taggingClient.TagResources(ctx, &rgta.TagResourcesInput{
		ResourceARNList: []string{*arn},
		Tags:            map[string]string{"via": "rgta"},
	})
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatal(tagged, err)
	}

	for _, other := range []*sdk.Client{dmsClient(s, "team-b", "us-east-1"), dmsClient(s, "test", "eu-west-1")} {
		list, e := other.DescribeEndpoints(ctx, &sdk.DescribeEndpointsInput{})
		if e != nil || len(list.Endpoints) != 0 {
			t.Fatal(list, e)
		}

		groups, e := other.DescribeReplicationSubnetGroups(ctx, &sdk.DescribeReplicationSubnetGroupsInput{})
		if e != nil || len(groups.ReplicationSubnetGroups) != 0 {
			t.Fatal(groups, e)
		}

		instances, e := other.DescribeReplicationInstances(ctx, &sdk.DescribeReplicationInstancesInput{})
		if e != nil || len(instances.ReplicationInstances) != 0 {
			t.Fatal(instances, e)
		}

		tasks, e := other.DescribeReplicationTasks(ctx, &sdk.DescribeReplicationTasksInput{})
		if e != nil || len(tasks.ReplicationTasks) != 0 {
			t.Fatal(tasks, e)
		}

		_, e = other.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: arn})
		wantDMSError(t, e, "ResourceNotFoundFault")
		_, e = other.DeleteReplicationTask(ctx, &sdk.DeleteReplicationTaskInput{ReplicationTaskArn: taskArn})
		wantDMSError(t, e, "ResourceNotFoundFault")
	}

	for _, path := range []string{"/_tinystack/requests", "/_tinystack/resources"} {
		response, e := http.Get(s.URL + path)
		if e != nil {
			t.Fatal(e)
		}

		body, e := io.ReadAll(response.Body)
		response.Body.Close()
		if e != nil || strings.Contains(string(body), "dms-secret-") {
			t.Fatalf("secret leaked at %s: %s %v", path, body, e)
		}
	}

	_, err = c.DeleteReplicationTask(ctx, &sdk.DeleteReplicationTaskInput{ReplicationTaskArn: taskArn})
	if err != nil {
		t.Fatal(err)
	}

	for _, endpoint := range []*string{source.Endpoint.EndpointArn, target.Endpoint.EndpointArn} {
		_, err = c.DeleteEndpoint(ctx, &sdk.DeleteEndpointInput{EndpointArn: endpoint})
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.DeleteReplicationSubnetGroup(ctx, &sdk.DeleteReplicationSubnetGroupInput{
		ReplicationSubnetGroupIdentifier: subnetIn.ReplicationSubnetGroupIdentifier,
	})
	wantDMSError(t, err, "InvalidResourceStateFault")
	_, err = c.DeleteReplicationInstance(ctx, &sdk.DeleteReplicationInstanceInput{ReplicationInstanceArn: arn})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteReplicationSubnetGroup(ctx, &sdk.DeleteReplicationSubnetGroupInput{
		ReplicationSubnetGroupIdentifier: subnetIn.ReplicationSubnetGroupIdentifier,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDMSSDKValidationAndPagination(t *testing.T) {
	s := server(t)
	c := dmsClient(s, "test", "us-east-1")
	ctx := context.Background()
	subnets, _ := dmsNetwork(t, s)
	for _, ids := range [][]string{{subnets[0]}, {subnets[0], subnets[2]}, {"subnet-missing", subnets[1]}} {
		_, err := c.CreateReplicationSubnetGroup(ctx, &sdk.CreateReplicationSubnetGroupInput{
			ReplicationSubnetGroupIdentifier: new(
				"invalid",
			),
			ReplicationSubnetGroupDescription: new("local"),
			SubnetIds:                         ids,
		})
		code := "ReplicationSubnetGroupDoesNotCoverEnoughAZs"
		if strings.Contains(ids[0], "missing") {
			code = "InvalidSubnet"
		}

		wantDMSError(t, err, code)
	}

	otherVpc, err := ec2Client(
		s,
		"test",
		"us-east-1",
	).CreateVpc(ctx, &ec2sdk.CreateVpcInput{CidrBlock: new("10.36.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}

	otherSubnet, err := ec2Client(s, "test", "us-east-1").CreateSubnet(ctx, &ec2sdk.CreateSubnetInput{
		VpcId: otherVpc.Vpc.VpcId, CidrBlock: new("10.36.0.0/24"), AvailabilityZone: new("us-east-1b"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateReplicationSubnetGroup(ctx, &sdk.CreateReplicationSubnetGroupInput{
		ReplicationSubnetGroupIdentifier: new("invalid"), ReplicationSubnetGroupDescription: new("local"),
		SubnetIds: []string{subnets[0], *otherSubnet.Subnet.SubnetId},
	})
	wantDMSError(t, err, "InvalidSubnet")
	_, err = c.CreateReplicationSubnetGroup(ctx, &sdk.CreateReplicationSubnetGroupInput{
		ReplicationSubnetGroupIdentifier: new(
			"valid",
		),
		ReplicationSubnetGroupDescription: new("local"),
		SubnetIds:                         subnets[:2],
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateReplicationInstance(ctx, &sdk.CreateReplicationInstanceInput{
		ReplicationInstanceIdentifier: new("bad-groups"),
		ReplicationInstanceClass:      new("dms.t3.micro"), ReplicationSubnetGroupIdentifier: new("valid"),
		VpcSecurityGroupIds: []string{"sg-missing"},
	})
	wantDMSError(t, err, "InvalidParameterValueException")
	for i := range 23 {
		_, err = c.CreateEndpoint(ctx, &sdk.CreateEndpointInput{
			EndpointIdentifier: new(fmt.Sprintf("source-%02d", i)),
			EndpointType:       types.ReplicationEndpointTypeValueSource, EngineName: new("mysql"),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	first, err := c.DescribeEndpoints(
		ctx,
		&sdk.DescribeEndpointsInput{MaxRecords: new(int32(20)), Filters: []types.Filter{
			{Name: new("engine-name"), Values: []string{"mysql", "postgres"}},
		}},
	)
	if err != nil || len(first.Endpoints) != 20 || first.Marker == nil {
		t.Fatal(first, err)
	}

	second, err := c.DescribeEndpoints(
		ctx,
		&sdk.DescribeEndpointsInput{MaxRecords: new(int32(20)), Marker: first.Marker},
	)
	if err != nil || len(second.Endpoints) != 3 || second.Marker != nil {
		t.Fatal(second, err)
	}

	seen := map[string]bool{}
	for _, e := range append(first.Endpoints, second.Endpoints...) {
		if seen[*e.EndpointArn] {
			t.Fatal("duplicate page item")
		}

		seen[*e.EndpointArn] = true
	}

	for _, in := range []*sdk.DescribeEndpointsInput{
		{MaxRecords: new(int32(1))},
		{Marker: new("!")},
		{Filters: []types.Filter{{Name: new("unknown"), Values: []string{"x"}}}},
	} {
		_, err = c.DescribeEndpoints(ctx, in)
		wantDMSError(t, err, "InvalidParameterValueException")
	}

	_, err = c.AddTagsToResource(ctx, &sdk.AddTagsToResourceInput{
		ResourceArn: first.Endpoints[0].EndpointArn,
		Tags:        []types.Tag{{Key: new("aws:reserved"), Value: new("no")}},
	})
	wantDMSError(t, err, "InvalidParameterValueException")
	_, err = c.CreateEndpoint(ctx, &sdk.CreateEndpointInput{
		EndpointIdentifier: new("unsupported"),
		EndpointType:       types.ReplicationEndpointTypeValueSource,
		EngineName:         new("postgres"),
		CertificateArn:     new("cert"),
	})
	wantDMSError(t, err, "ValidationException")

	// Serialize a response as an additional assertion that the SDK sees JSON 1.1 data.
	data, err := json.Marshal(first.Endpoints[0])
	if err != nil || strings.Contains(string(data), "Password") {
		t.Fatal(string(data), err)
	}
}
