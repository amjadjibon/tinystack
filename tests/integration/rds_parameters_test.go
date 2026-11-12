package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
)

func TestRDSParameters(t *testing.T) {
	c := rdsClient(server(t), "test", "us-east-1")
	ctx := context.Background()
	_, err := c.CreateDBParameterGroup(
		ctx,
		&rds.CreateDBParameterGroupInput{
			DBParameterGroupName:   new("custom"),
			DBParameterGroupFamily: new("postgres16"),
			Description:            new("test"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	parameter := types.Parameter{
		ParameterName:  new("work_mem"),
		ParameterValue: new("8192"),
		ApplyMethod:    types.ApplyMethodImmediate,
	}

	_, err = c.ModifyDBParameterGroup(
		ctx,
		&rds.ModifyDBParameterGroupInput{DBParameterGroupName: new("custom"), Parameters: []types.Parameter{parameter}},
	)
	if err != nil {
		t.Fatal(err)
	}

	out, err := c.DescribeDBParameters(
		ctx,
		&rds.DescribeDBParametersInput{DBParameterGroupName: new("custom"), Source: new("user")},
	)
	if err != nil || len(out.Parameters) != 1 || aws.ToString(out.Parameters[0].ParameterValue) != "8192" {
		t.Fatalf("describe: %+v %v", out, err)
	}

	for _, p := range []types.Parameter{
		{ParameterName: new("unknown"), ParameterValue: new("1"), ApplyMethod: types.ApplyMethodImmediate},
		{ParameterName: new("work_mem"), ParameterValue: new("-1"), ApplyMethod: types.ApplyMethodImmediate},
		{ParameterName: new("max_connections"), ParameterValue: new("200"), ApplyMethod: types.ApplyMethodImmediate},
		{ParameterName: new("server_version"), ParameterValue: new("17"), ApplyMethod: types.ApplyMethodPendingReboot},
	} {
		_, err = c.ModifyDBParameterGroup(
			ctx,
			&rds.ModifyDBParameterGroupInput{DBParameterGroupName: new("custom"), Parameters: []types.Parameter{p}},
		)
		code(t, err, "InvalidParameterValue")
	}

	parameters := make([]types.Parameter, 21)
	for i := range parameters {
		parameters[i] = parameter
	}

	_, err = c.ModifyDBParameterGroup(
		ctx,
		&rds.ModifyDBParameterGroupInput{DBParameterGroupName: new("custom"), Parameters: parameters},
	)
	code(t, err, "InvalidParameterValue")
	first, err := c.DescribeDBParameters(
		ctx,
		&rds.DescribeDBParametersInput{DBParameterGroupName: new("custom"), MaxRecords: new(int32(20))},
	)
	if err != nil || len(first.Parameters) != 20 || first.Marker == nil {
		t.Fatalf("page: %+v %v", first, err)
	}

	second, err := c.DescribeDBParameters(
		ctx,
		&rds.DescribeDBParametersInput{
			DBParameterGroupName: new("custom"),
			Marker:               first.Marker,
			MaxRecords:           new(int32(20)),
		},
	)
	if err != nil || len(second.Parameters) != 1 {
		t.Fatalf("second page: %+v %v", second, err)
	}

	_, err = c.CopyDBParameterGroup(
		ctx,
		&rds.CopyDBParameterGroupInput{
			SourceDBParameterGroupIdentifier:  new("custom"),
			TargetDBParameterGroupIdentifier:  new("copy"),
			TargetDBParameterGroupDescription: new("copy"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ResetDBParameterGroup(
		ctx,
		&rds.ResetDBParameterGroupInput{
			DBParameterGroupName: new("custom"),
			Parameters: []types.Parameter{
				{ParameterName: new("work_mem"), ApplyMethod: types.ApplyMethodImmediate},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	out, err = c.DescribeDBParameters(
		ctx,
		&rds.DescribeDBParametersInput{DBParameterGroupName: new("custom"), Source: new("user")},
	)
	if err != nil || len(out.Parameters) != 0 {
		t.Fatalf("reset: %+v %v", out, err)
	}

	_, err = c.ResetDBParameterGroup(
		ctx,
		&rds.ResetDBParameterGroupInput{DBParameterGroupName: new("copy"), ResetAllParameters: new(true)},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ModifyDBParameterGroup(
		ctx,
		&rds.ModifyDBParameterGroupInput{
			DBParameterGroupName: new("default.postgres16"),
			Parameters:           []types.Parameter{parameter},
		},
	)
	code(t, err, "InvalidParameterValue")
	defaults, err := c.DescribeEngineDefaultParameters(
		ctx,
		&rds.DescribeEngineDefaultParametersInput{DBParameterGroupFamily: new("postgres16")},
	)
	if err != nil || len(defaults.EngineDefaults.Parameters) != 21 {
		t.Fatalf("defaults: %+v %v", defaults, err)
	}

	in := rdsInput("parameter-db")
	in.DBParameterGroupName = new("custom")
	_, err = c.CreateDBInstance(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteDBParameterGroup(ctx, &rds.DeleteDBParameterGroupInput{DBParameterGroupName: new("custom")})
	code(t, err, "InvalidDBParameterGroupState")
}

func TestRDSClusterParameters(t *testing.T) {
	c := rdsClient(server(t), "test", "us-east-1")
	ctx := context.Background()
	_, err := c.CreateDBClusterParameterGroup(
		ctx,
		&rds.CreateDBClusterParameterGroupInput{
			DBClusterParameterGroupName: new("cluster-custom"),
			DBParameterGroupFamily:      new("aurora-postgresql16"),
			Description:                 new("test"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ModifyDBClusterParameterGroup(
		ctx,
		&rds.ModifyDBClusterParameterGroupInput{
			DBClusterParameterGroupName: new("cluster-custom"),
			Parameters: []types.Parameter{
				{
					ParameterName:  new("max_connections"),
					ParameterValue: new("200"),
					ApplyMethod:    types.ApplyMethodPendingReboot,
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	out, err := c.DescribeDBClusterParameters(
		ctx,
		&rds.DescribeDBClusterParametersInput{DBClusterParameterGroupName: new("cluster-custom"), Source: new("user")},
	)
	if err != nil || len(out.Parameters) != 1 {
		t.Fatalf("describe: %+v %v", out, err)
	}

	_, err = c.CreateDBCluster(
		ctx,
		&rds.CreateDBClusterInput{
			DBClusterIdentifier:         new("parameter-cluster"),
			Engine:                      new("aurora-postgresql"),
			EngineVersion:               new("16.6"),
			MasterUsername:              new("testuser"),
			MasterUserPassword:          new("password123"),
			DBClusterParameterGroupName: new("cluster-custom"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteDBClusterParameterGroup(
		ctx,
		&rds.DeleteDBClusterParameterGroupInput{DBClusterParameterGroupName: new("cluster-custom")},
	)
	code(t, err, "InvalidDBParameterGroupState")
	_, err = c.ResetDBClusterParameterGroup(
		ctx,
		&rds.ResetDBClusterParameterGroupInput{
			DBClusterParameterGroupName: new("cluster-custom"),
			ResetAllParameters:          new(true),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeDBClusterParameterGroups(
		ctx,
		&rds.DescribeDBClusterParameterGroupsInput{DBClusterParameterGroupName: new("default.aurora-postgresql16")},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRDSParameterCloudFormation(t *testing.T) {
	server := server(t)
	c := cfnClient(server)
	ctx := context.Background()
	template := `{"Resources":{
 "Parameters":{"Type":"AWS::RDS::DBParameterGroup","Properties":{
 "Family":"postgres16","Description":"test","Parameters":{"work_mem":"8192"},
 "Tags":[{"Key":"test","Value":"yes"}]}},
 "ClusterParameters":{"Type":"AWS::RDS::DBClusterParameterGroup","Properties":{
 "Family":"aurora-mysql8.0","Description":"test","Parameters":{"max_connections":"200"}}},
 "Database":{"Type":"AWS::RDS::DBInstance","Properties":{
 "DBInstanceIdentifier":"parameter-cfn","DBInstanceClass":"db.t3.micro","Engine":"postgres",
 "MasterUsername":"master","MasterUserPassword":"password123","DBParameterGroupName":{"Ref":"Parameters"}}}},
 "Outputs":{"Group":{"Value":{"Ref":"Parameters"}},"ClusterGroup":{"Value":{"Ref":"ClusterParameters"}}}}`
	_, err := c.CreateStack(ctx, &cfn.CreateStackInput{StackName: new("parameter-stack"), TemplateBody: new(template)})
	if err != nil {
		t.Fatal(err)
	}

	stack := wantStatus(ctx, t, c, "parameter-stack", cfntypes.StackStatusCreateComplete)
	db := rdsClient(server, "test", "us-east-1")
	for _, output := range stack.Outputs {
		if aws.ToString(output.OutputKey) != "Group" {
			continue
		}

		out, describeErr := db.DescribeDBParameters(
			ctx,
			&rds.DescribeDBParametersInput{DBParameterGroupName: output.OutputValue, Source: new("user")},
		)
		if describeErr != nil || len(out.Parameters) != 1 {
			t.Fatalf("parameters: %+v %v", out, describeErr)
		}
	}

	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("parameter-stack")})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "parameter stack deletion", func() bool {
		_, describeErr := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: new("parameter-stack")})
		return describeErr != nil
	})
}
