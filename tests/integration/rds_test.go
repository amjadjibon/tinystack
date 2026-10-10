package integration_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
)

func rdsClient(s *httptest.Server, key, region string) *rds.Client {
	return rds.New(rds.Options{
		Region: region, BaseEndpoint: new(s.URL), Retryer: aws.NopRetryer{},
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
		}),
	})
}

func rdsInput(id string) *rds.CreateDBInstanceInput {
	return &rds.CreateDBInstanceInput{
		DBInstanceIdentifier: new(id),
		DBInstanceClass:      new("db.t3.micro"),
		Engine: new(
			"postgres",
		),
		MasterUsername:     new("testuser"),
		MasterUserPassword: new("password123"),
		DBName:             new("testdb"),
	}
}

func TestRDSLifecycle(t *testing.T) {
	s := server(t)
	c := rdsClient(s, "test", "us-east-1")
	ctx := context.Background()
	in := rdsInput("test-db")
	in.Tags = []types.Tag{{Key: new("initial"), Value: new("yes")}}
	out, err := c.CreateDBInstance(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	db := out.DBInstance
	if aws.ToString(db.DBInstanceStatus) != "available" || aws.ToString(db.EngineVersion) != "16" ||
		aws.ToString(db.Endpoint.Address) != "127.0.0.1" || aws.ToInt32(db.Endpoint.Port) != 5432 ||
		aws.ToString(db.DBInstanceArn) != "arn:aws:rds:us-east-1:000000000000:db:test-db" {
		t.Fatalf("instance: %+v", db)
	}

	_, err = c.CreateDBInstance(ctx, in)
	code(t, err, "DBInstanceAlreadyExists")
	_, err = c.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{DBInstanceIdentifier: new("missing")})
	code(t, err, "DBInstanceNotFound")
	modified, err := c.ModifyDBInstance(ctx, &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier: new("test-db"),
		DBInstanceClass: new(
			"db.t3.small",
		),
		MasterUserPassword: new("newpassword123"),
		ApplyImmediately:   new(true),
	})
	if err != nil || aws.ToString(modified.DBInstance.DBInstanceClass) != "db.t3.small" {
		t.Fatalf("modify: %v %+v", err, modified)
	}

	stopped, err := c.StopDBInstance(ctx, &rds.StopDBInstanceInput{DBInstanceIdentifier: new("test-db")})
	if err != nil || aws.ToString(stopped.DBInstance.DBInstanceStatus) != "stopped" {
		t.Fatalf("stop: %v", err)
	}

	_, err = c.StopDBInstance(ctx, &rds.StopDBInstanceInput{DBInstanceIdentifier: new("test-db")})
	code(t, err, "InvalidDBInstanceState")
	_, err = c.RebootDBInstance(ctx, &rds.RebootDBInstanceInput{DBInstanceIdentifier: new("test-db")})
	code(t, err, "InvalidDBInstanceState")
	started, err := c.StartDBInstance(ctx, &rds.StartDBInstanceInput{DBInstanceIdentifier: new("test-db")})
	if err != nil || aws.ToString(started.DBInstance.DBInstanceStatus) != "available" {
		t.Fatalf("start: %v", err)
	}

	_, err = c.RebootDBInstance(ctx, &rds.RebootDBInstanceInput{DBInstanceIdentifier: new("test-db")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{DBInstanceIdentifier: new("test-db")})
	code(t, err, "InvalidParameterValue")
	_, err = c.DeleteDBInstance(
		ctx,
		&rds.DeleteDBInstanceInput{DBInstanceIdentifier: new("test-db"), SkipFinalSnapshot: new(true)},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteDBInstance(
		ctx,
		&rds.DeleteDBInstanceInput{DBInstanceIdentifier: new("test-db"), SkipFinalSnapshot: new(true)},
	)
	code(t, err, "DBInstanceNotFound")
}

func TestRDSPaginationIsolationFilters(t *testing.T) {
	s := server(t)
	c := rdsClient(s, "test", "us-east-1")
	ctx := context.Background()
	for i := range 23 {
		in := rdsInput(fmt.Sprintf("db-%02d", i))
		if i == 22 {
			in.Engine = new("mysql")
		}

		if _, err := c.CreateDBInstance(ctx, in); err != nil {
			t.Fatal(err)
		}
	}

	first, err := c.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{MaxRecords: new(int32(20))})
	if err != nil || len(first.DBInstances) != 20 || first.Marker == nil {
		t.Fatalf("first: %v %+v", err, first)
	}

	next, err := c.DescribeDBInstances(
		ctx,
		&rds.DescribeDBInstancesInput{MaxRecords: new(int32(20)), Marker: first.Marker},
	)
	if err != nil || len(next.DBInstances) != 3 || next.Marker != nil {
		t.Fatalf("next: %v %+v", err, next)
	}

	filtered, err := c.DescribeDBInstances(
		ctx,
		&rds.DescribeDBInstancesInput{Filters: []types.Filter{{Name: new("engine"), Values: []string{"mysql"}}}},
	)
	if err != nil || len(filtered.DBInstances) != 1 {
		t.Fatalf("filter: %v %+v", err, filtered)
	}

	_, err = c.DescribeDBInstances(
		ctx,
		&rds.DescribeDBInstancesInput{Filters: []types.Filter{{Name: new("db-cluster-id"), Values: []string{"x"}}}},
	)
	code(t, err, "InvalidParameterValue")
	_, err = c.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{Marker: new("invalid!")})
	code(t, err, "InvalidParameterValue")
	_, err = c.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{MaxRecords: new(int32(0))})
	code(t, err, "InvalidParameterValue")
	for _, other := range []*rds.Client{rdsClient(s, "team-b", "us-east-1"), rdsClient(s, "test", "eu-west-1")} {
		list, e := other.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{})
		if e != nil || len(list.DBInstances) != 0 {
			t.Fatalf("isolation: %v", e)
		}

		if _, e = other.CreateDBInstance(ctx, rdsInput("db-00")); e != nil {
			t.Fatal(e)
		}
	}
}

func TestRDSUnsupportedInputs(t *testing.T) {
	s := server(t)
	c := rdsClient(s, "test", "us-east-1")
	ctx := context.Background()
	for _, engine := range []string{"aurora-postgresql", "oracle-ee", "sqlserver-ee", "redis"} {
		in := rdsInput("unsupported")
		in.Engine = new(engine)
		_, err := c.CreateDBInstance(ctx, in)
		code(t, err, "InvalidParameterValue")
	}

	for _, version := range []string{"13", "16.1", "99"} {
		in := rdsInput("unsupported")
		in.EngineVersion = new(version)
		_, err := c.CreateDBInstance(ctx, in)
		code(t, err, "InvalidParameterValue")
	}

	cases := []func(*rds.CreateDBInstanceInput){
		func(in *rds.CreateDBInstanceInput) { in.MultiAZ = new(true) },
		func(in *rds.CreateDBInstanceInput) { in.EnableIAMDatabaseAuthentication = new(true) },
		func(in *rds.CreateDBInstanceInput) { in.ManageMasterUserPassword = new(true) },
		func(in *rds.CreateDBInstanceInput) { in.OptionGroupName = new("options") },
		func(in *rds.CreateDBInstanceInput) { in.DBClusterIdentifier = new("cluster") },
	}

	for _, change := range cases {
		in := rdsInput("unsupported")
		change(in)
		_, err := c.CreateDBInstance(ctx, in)
		code(t, err, "InvalidParameterValue")
	}

	in := rdsInput("bad--id")
	_, err := c.CreateDBInstance(ctx, in)
	code(t, err, "InvalidParameterValue")
	in = rdsInput("bad-password")
	in.MasterUserPassword = new("short")
	_, err = c.CreateDBInstance(ctx, in)
	code(t, err, "InvalidParameterValue")
	_, err = c.CreateDBInstance(ctx, rdsInput("good"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ModifyDBInstance(
		ctx,
		&rds.ModifyDBInstanceInput{DBInstanceIdentifier: new("good"), ApplyImmediately: new(false)},
	)
	code(t, err, "InvalidParameterValue")
	_, err = c.ModifyDBInstance(ctx, &rds.ModifyDBInstanceInput{DBInstanceIdentifier: new("good"), MultiAZ: new(true)})
	code(t, err, "InvalidParameterValue")
	_, err = c.RebootDBInstance(
		ctx,
		&rds.RebootDBInstanceInput{DBInstanceIdentifier: new("good"), ForceFailover: new(true)},
	)
	code(t, err, "InvalidParameterValue")
	// Raw Query responses exercise the XML wrapper, namespace and modeled wire error code.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, strings.NewReader(
		"Action=DescribeDBInstances&Version=2014-10-31&DBInstanceIdentifier=missing"))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != 404 || !strings.Contains(string(body), "<Code>DBInstanceNotFound</Code>") {
		t.Fatalf("raw error: %d %s", resp.StatusCode, body)
	}
	successRequest, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, strings.NewReader(
		"Action=DescribeDBInstances&Version=2014-10-31&DBInstanceIdentifier=good"))
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	successRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	success, requestErr := http.DefaultClient.Do(successRequest)
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	defer success.Body.Close()
	body, err = io.ReadAll(success.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		"DescribeDBInstancesResponse", "DescribeDBInstancesResult",
		"http://rds.amazonaws.com/doc/2014-10-31/", "<DBInstanceIdentifier>good</DBInstanceIdentifier>",
		"<DBInstanceStatus>available</DBInstanceStatus>",
	} {
		if success.StatusCode != 200 || !strings.Contains(string(body), fragment) {
			t.Fatalf("raw success: %d %s", success.StatusCode, body)
		}
	}
	if strings.Contains(string(body), "password123") {
		t.Fatal("Query output exposed a password")
	}
}

func TestRDSTagsAndGroups(t *testing.T) {
	s := server(t)
	c := rdsClient(s, "test", "us-east-1")
	ctx := context.Background()
	_, subnets := cacheSubnets(t, s)
	subnet, err := c.CreateDBSubnetGroup(ctx, &rds.CreateDBSubnetGroupInput{
		DBSubnetGroupName:        new("subnets"),
		DBSubnetGroupDescription: new("test subnets"), SubnetIds: subnets,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(subnet.DBSubnetGroup.Subnets) != 2 {
		t.Fatal("subnets missing")
	}

	_, err = c.CreateDBSubnetGroup(ctx, &rds.CreateDBSubnetGroupInput{
		DBSubnetGroupName:        new("subnets"),
		DBSubnetGroupDescription: new("duplicate"), SubnetIds: subnets,
	})
	code(t, err, "DBSubnetGroupAlreadyExists")
	_, err = c.CreateDBSubnetGroup(ctx, &rds.CreateDBSubnetGroupInput{
		DBSubnetGroupName:        new("bad"),
		DBSubnetGroupDescription: new("invalid"), SubnetIds: []string{"subnet-missing"},
	})
	code(t, err, "InvalidSubnet")
	parameter, err := c.CreateDBParameterGroup(ctx, &rds.CreateDBParameterGroupInput{
		DBParameterGroupName:   new("params"),
		DBParameterGroupFamily: new("postgres16"), Description: new("test params"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateDBParameterGroup(ctx, &rds.CreateDBParameterGroupInput{
		DBParameterGroupName:   new("params"),
		DBParameterGroupFamily: new("postgres16"), Description: new("duplicate"),
	})
	code(t, err, "DBParameterGroupAlreadyExists")
	in := rdsInput("groups")
	in.DBSubnetGroupName = new("subnets")
	in.DBParameterGroupName = new("params")
	db, err := c.CreateDBInstance(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteDBSubnetGroup(ctx, &rds.DeleteDBSubnetGroupInput{DBSubnetGroupName: new("subnets")})
	code(t, err, "InvalidDBSubnetGroupStateFault")
	_, err = c.DeleteDBParameterGroup(ctx, &rds.DeleteDBParameterGroupInput{DBParameterGroupName: new("params")})
	code(t, err, "InvalidDBParameterGroupState")
	resources := []*string{
		db.DBInstance.DBInstanceArn,
		subnet.DBSubnetGroup.DBSubnetGroupArn,
		parameter.DBParameterGroup.DBParameterGroupArn,
	}

	for _, resource := range resources {
		_, err = c.AddTagsToResource(
			ctx,
			&rds.AddTagsToResourceInput{
				ResourceName: resource,
				Tags:         []types.Tag{{Key: new("env"), Value: new("test")}},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		tags, e := c.ListTagsForResource(ctx, &rds.ListTagsForResourceInput{ResourceName: resource})
		if e != nil || len(tags.TagList) != 1 {
			t.Fatalf("tags: %v %+v", e, tags)
		}

		_, err = rdsClient(
			s,
			"team-b",
			"us-east-1",
		).ListTagsForResource(ctx, &rds.ListTagsForResourceInput{ResourceName: resource})
		code(t, err, "InvalidParameterValue")
		_, err = c.RemoveTagsFromResource(
			ctx,
			&rds.RemoveTagsFromResourceInput{ResourceName: resource, TagKeys: []string{"env"}},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	// The cross-service tagging API locates RDS inventory and invokes its tag operations.
	taggingClient := resourcegroupstaggingapi.New(resourcegroupstaggingapi.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL),
		Credentials: aws.AnonymousCredentials{}, Retryer: aws.NopRetryer{},
	})
	tagged, err := taggingClient.TagResources(ctx, &resourcegroupstaggingapi.TagResourcesInput{
		ResourceARNList: []string{aws.ToString(db.DBInstance.DBInstanceArn)}, Tags: map[string]string{"shared": "yes"},
	})
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("tagging API: %v %+v", err, tagged)
	}

	groups, err := c.DescribeDBSubnetGroups(ctx, &rds.DescribeDBSubnetGroupsInput{})
	if err != nil || len(groups.DBSubnetGroups) != 1 {
		t.Fatalf("subnets: %v", err)
	}

	params, err := c.DescribeDBParameterGroups(ctx, &rds.DescribeDBParameterGroupsInput{})
	if err != nil || len(params.DBParameterGroups) != 1 {
		t.Fatalf("params: %v", err)
	}

	_, err = c.DeleteDBInstance(
		ctx,
		&rds.DeleteDBInstanceInput{DBInstanceIdentifier: new("groups"), SkipFinalSnapshot: new(true)},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteDBSubnetGroup(ctx, &rds.DeleteDBSubnetGroupInput{DBSubnetGroupName: new("subnets")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteDBParameterGroup(ctx, &rds.DeleteDBParameterGroupInput{DBParameterGroupName: new("params")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeDBSubnetGroups(ctx, &rds.DescribeDBSubnetGroupsInput{DBSubnetGroupName: new("subnets")})
	code(t, err, "DBSubnetGroupNotFoundFault")
	_, err = c.DescribeDBParameterGroups(ctx, &rds.DescribeDBParameterGroupsInput{DBParameterGroupName: new("params")})
	code(t, err, "DBParameterGroupNotFound")
}

func TestRDSPersistentRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := rdsClient(s, "test", "us-east-1")
	if _, err := c.CreateDBInstance(ctx, rdsInput("persisted")); err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	out, err := rdsClient(
		s,
		"test",
		"us-east-1",
	).DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{DBInstanceIdentifier: new("persisted")})
	if err != nil || len(out.DBInstances) != 1 || aws.ToString(out.DBInstances[0].DBInstanceStatus) != "available" {
		t.Fatalf("restart: %v %+v", err, out)
	}
}

func TestRDSCloudFormation(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cfnClient(s)
	_, subnets := cacheSubnets(t, s)
	template := fmt.Sprintf(`{"Resources":{
 "SubnetGroup":{"Type":"AWS::RDS::DBSubnetGroup","Properties":{
 "DBSubnetGroupName":"cfn-subnets","DBSubnetGroupDescription":"test","SubnetIds":[%q,%q]}},
 "Database":{"Type":"AWS::RDS::DBInstance","Properties":{
 "DBInstanceIdentifier":"cfn-db","DBInstanceClass":"db.t3.micro","Engine":"postgres",
 "MasterUsername":"master","MasterUserPassword":"password123","DBSubnetGroupName":{"Ref":"SubnetGroup"}}}},
 "Outputs":{"Host":{"Value":{"Fn::GetAtt":["Database","Endpoint.Address"]}},
 "Port":{"Value":{"Fn::GetAtt":["Database","Endpoint.Port"]}}}}`, subnets[0], subnets[1])
	if _, err := c.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("rds-stack"), TemplateBody: new(template)},
	); err != nil {
		t.Fatal(err)
	}

	stack := wantStatus(ctx, t, c, "rds-stack", cfntypes.StackStatusCreateComplete)
	if len(stack.Outputs) != 2 {
		t.Fatalf("outputs: %+v", stack.Outputs)
	}

	if _, err := c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("rds-stack")}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "RDS stack deletion", func() bool {
		_, err := rdsClient(
			s,
			"test",
			"us-east-1",
		).DescribeDBSubnetGroups(ctx, &rds.DescribeDBSubnetGroupsInput{DBSubnetGroupName: new("cfn-subnets")})
		return err != nil
	})
	_, err := rdsClient(
		s,
		"test",
		"us-east-1",
	).DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{DBInstanceIdentifier: new("cfn-db")})
	code(t, err, "DBInstanceNotFound")
}

func TestRDSGroupPagination(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := rdsClient(s, "test", "us-east-1")
	_, subnets := cacheSubnets(t, s)
	for i := range 21 {
		name := fmt.Sprintf("group-%02d", i)
		if _, err := c.CreateDBSubnetGroup(ctx, &rds.CreateDBSubnetGroupInput{
			DBSubnetGroupName:        new(name),
			DBSubnetGroupDescription: new("test"), SubnetIds: subnets,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.CreateDBParameterGroup(ctx, &rds.CreateDBParameterGroupInput{
			DBParameterGroupName:   new(name),
			DBParameterGroupFamily: new("postgres16"), Description: new("test"),
		}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := c.DescribeDBSubnetGroups(ctx, &rds.DescribeDBSubnetGroupsInput{MaxRecords: new(int32(20))})
	if err != nil || len(first.DBSubnetGroups) != 20 || first.Marker == nil {
		t.Fatalf("subnet first: %v %+v", err, first)
	}
	next, err := c.DescribeDBSubnetGroups(
		ctx,
		&rds.DescribeDBSubnetGroupsInput{MaxRecords: new(int32(20)), Marker: first.Marker},
	)
	if err != nil || len(next.DBSubnetGroups) != 1 || next.Marker != nil {
		t.Fatalf("subnet next: %v %+v", err, next)
	}
	parameters, err := c.DescribeDBParameterGroups(ctx, &rds.DescribeDBParameterGroupsInput{MaxRecords: new(int32(20))})
	if err != nil || len(parameters.DBParameterGroups) != 20 || parameters.Marker == nil {
		t.Fatalf("parameter first: %v %+v", err, parameters)
	}
	rest, err := c.DescribeDBParameterGroups(
		ctx,
		&rds.DescribeDBParameterGroupsInput{MaxRecords: new(int32(20)), Marker: parameters.Marker},
	)
	if err != nil || len(rest.DBParameterGroups) != 1 || rest.Marker != nil {
		t.Fatalf("parameter next: %v %+v", err, rest)
	}
	_, err = c.DescribeDBSubnetGroups(
		ctx,
		&rds.DescribeDBSubnetGroupsInput{Filters: []types.Filter{{Name: new("vpc-id"), Values: []string{"x"}}}},
	)
	code(t, err, "InvalidParameterValue")
	_, err = c.DescribeDBParameterGroups(ctx, &rds.DescribeDBParameterGroupsInput{Marker: new("!")})
	code(t, err, "InvalidParameterValue")
}

func TestRDSEngineProfiles(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := rdsClient(s, "test", "us-east-1")
	for _, tc := range []struct {
		engine, version string
		port            int32
	}{
		{"postgres", "14", 5432}, {"mysql", "8.0", 3306}, {"mariadb", "11", 3306},
	} {
		in := rdsInput(tc.engine)
		in.Engine = new(tc.engine)
		in.EngineVersion = new(tc.version)
		out, err := c.CreateDBInstance(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if aws.ToString(out.DBInstance.EngineVersion) != tc.version ||
			aws.ToInt32(out.DBInstance.Endpoint.Port) != tc.port {
			t.Fatalf("engine profile: %+v", out.DBInstance)
		}
		in.DBInstanceIdentifier = new(tc.engine + "-unsupported")
		in.EngineVersion = new("99")
		_, err = c.CreateDBInstance(ctx, in)
		code(t, err, "InvalidParameterValue")
	}
}
