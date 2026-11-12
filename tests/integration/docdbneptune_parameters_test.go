package integration_test

import (
	"context"
	"strings"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/docdb"
	docdbtypes "github.com/aws/aws-sdk-go-v2/service/docdb/types"
	"github.com/aws/aws-sdk-go-v2/service/neptune"
	neptunetypes "github.com/aws/aws-sdk-go-v2/service/neptune/types"
)

func TestDocDBClusterParameters(t *testing.T) {
	s := server(t)
	c := docdb.New(docdb.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	ctx := context.Background()
	for name, family := range map[string]string{"docdb3": "docdb3.6", "docdb5": "docdb5.0"} {
		if _, err := c.CreateDBClusterParameterGroup(ctx, &docdb.CreateDBClusterParameterGroupInput{
			DBClusterParameterGroupName: new(name), DBParameterGroupFamily: new(family),
			Description: new("test"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// FIPS TLS needs DocumentDB 5.0, and tls is static.
	modify := func(group, name, value string, method docdbtypes.ApplyMethod) error {
		_, err := c.ModifyDBClusterParameterGroup(ctx, &docdb.ModifyDBClusterParameterGroupInput{
			DBClusterParameterGroupName: new(group),
			Parameters: []docdbtypes.Parameter{
				{ParameterName: new(name), ParameterValue: new(value), ApplyMethod: method},
			},
		})
		return err
	}

	code(t, modify("docdb3", "tls", "fips-140-3", docdbtypes.ApplyMethodPendingReboot), "InvalidParameterValue")
	code(t, modify("docdb5", "tls", "disabled", docdbtypes.ApplyMethodImmediate), "InvalidParameterValue")
	code(t, modify("docdb5", "change_stream_log_retention_duration", "3600", docdbtypes.ApplyMethodImmediate),
		"InvalidParameterValue")
	if err := modify("docdb5", "tls", "fips-140-3", docdbtypes.ApplyMethodPendingReboot); err != nil {
		t.Fatal(err)
	}

	if err := modify("docdb5", "profiler", "enabled", docdbtypes.ApplyMethodImmediate); err != nil {
		t.Fatal(err)
	}

	out, err := c.DescribeDBClusterParameters(ctx, &docdb.DescribeDBClusterParametersInput{
		DBClusterParameterGroupName: new("docdb5"), Source: new("user"),
	})
	if err != nil || len(out.Parameters) != 2 {
		t.Fatalf("user parameters: %+v %v", out, err)
	}

	defaults, err := c.DescribeEngineDefaultClusterParameters(ctx, &docdb.DescribeEngineDefaultClusterParametersInput{
		DBParameterGroupFamily: new("docdb4.0"),
	})
	if err != nil || len(defaults.EngineDefaults.Parameters) != 6 {
		t.Fatalf("defaults: %+v %v", defaults, err)
	}

	// The cluster's engine version must match the group's family.
	_, err = c.CreateDBCluster(ctx, &docdb.CreateDBClusterInput{
		DBClusterIdentifier: new("mismatch"), Engine: new("docdb"), EngineVersion: new("4.0"),
		MasterUsername: new("master"), MasterUserPassword: new("password123"),
		DBClusterParameterGroupName: new("docdb5"),
	})
	code(t, err, "InvalidParameterValue")
	cluster, err := c.CreateDBCluster(ctx, &docdb.CreateDBClusterInput{
		DBClusterIdentifier: new("documents"), Engine: new("docdb"), MasterUsername: new("master"),
		MasterUserPassword: new("password123"), DBClusterParameterGroupName: new("docdb5"),
	})
	if err != nil || *cluster.DBCluster.DBClusterParameterGroup != "docdb5" {
		t.Fatalf("cluster: %+v %v", cluster, err)
	}

	_, err = c.DeleteDBClusterParameterGroup(ctx, &docdb.DeleteDBClusterParameterGroupInput{
		DBClusterParameterGroupName: new("docdb5"),
	})
	code(t, err, "InvalidDBParameterGroupState")
	if _, err = c.ModifyDBCluster(ctx, &docdb.ModifyDBClusterInput{
		DBClusterIdentifier: new("documents"), DBClusterParameterGroupName: new("default.docdb5.0"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteDBClusterParameterGroup(ctx, &docdb.DeleteDBClusterParameterGroupInput{
		DBClusterParameterGroupName: new("docdb5"),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNeptuneClusterParameters(t *testing.T) {
	s := server(t)
	c := neptune.New(neptune.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	ctx := context.Background()
	if _, err := c.CreateDBClusterParameterGroup(ctx, &neptune.CreateDBClusterParameterGroupInput{
		DBClusterParameterGroupName: new("graph"), DBParameterGroupFamily: new("neptune1.3"),
		Description: new("test"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err := c.CreateDBClusterParameterGroup(ctx, &neptune.CreateDBClusterParameterGroupInput{
		DBClusterParameterGroupName: new("old"), DBParameterGroupFamily: new("neptune1"), Description: new("test"),
	})
	code(t, err, "InvalidParameterValue")
	for _, p := range []neptunetypes.Parameter{
		{
			ParameterName: new("neptune_query_timeout"), ParameterValue: new("5"),
			ApplyMethod: neptunetypes.ApplyMethodPendingReboot,
		},
		{
			ParameterName: new("neptune_query_timeout"), ParameterValue: new("5000"),
			ApplyMethod: neptunetypes.ApplyMethodImmediate,
		},
	} {
		_, err = c.ModifyDBClusterParameterGroup(ctx, &neptune.ModifyDBClusterParameterGroupInput{
			DBClusterParameterGroupName: new("graph"), Parameters: []neptunetypes.Parameter{p},
		})
		code(t, err, "InvalidParameterValue")
	}

	if _, err = c.ModifyDBClusterParameterGroup(ctx, &neptune.ModifyDBClusterParameterGroupInput{
		DBClusterParameterGroupName: new("graph"),
		Parameters: []neptunetypes.Parameter{
			{
				ParameterName: new("neptune_query_timeout"), ParameterValue: new("5000"),
				ApplyMethod: neptunetypes.ApplyMethodPendingReboot,
			},
			{
				ParameterName: new("neptune_enable_slow_query_log"), ParameterValue: new("info"),
				ApplyMethod: neptunetypes.ApplyMethodImmediate,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateDBCluster(ctx, &neptune.CreateDBClusterInput{
		DBClusterIdentifier: new("old-graph"), Engine: new("neptune"), EngineVersion: new("1.2.1.0"),
		DBClusterParameterGroupName: new("graph"),
	})
	code(t, err, "InvalidParameterValue")
	cluster, err := c.CreateDBCluster(ctx, &neptune.CreateDBClusterInput{
		DBClusterIdentifier: new("graph"), Engine: new("neptune"), DBClusterParameterGroupName: new("graph"),
	})
	if err != nil || *cluster.DBCluster.DBClusterParameterGroup != "graph" {
		t.Fatalf("cluster: %+v %v", cluster, err)
	}

	out, err := c.DescribeDBClusterParameters(ctx, &neptune.DescribeDBClusterParametersInput{
		DBClusterParameterGroupName: new("default.neptune1.2"),
	})
	if err != nil || len(out.Parameters) != 6 {
		t.Fatalf("defaults: %+v %v", out, err)
	}
}

func TestDocDBNeptuneParameterCloudFormation(t *testing.T) {
	s := server(t)
	c := cfnClient(s)
	ctx := context.Background()
	template := `{"Resources":{
 "Documents":{"Type":"AWS::DocDB::DBClusterParameterGroup","Properties":{
 "Name":"documents","Family":"docdb5.0","Description":"test","Parameters":{"tls":"disabled"}}},
 "Graph":{"Type":"AWS::Neptune::DBClusterParameterGroup","Properties":{
 "Family":"neptune1.3","Description":"test","Parameters":{"neptune_query_timeout":5000}}},
 "Cluster":{"Type":"AWS::DocDB::DBCluster","Properties":{
 "DBClusterIdentifier":"documents","MasterUsername":"master","MasterUserPassword":"password123",
 "DBClusterParameterGroupName":{"Ref":"Documents"}}},
 "GraphCluster":{"Type":"AWS::Neptune::DBCluster","Properties":{
 "DBClusterIdentifier":"graph","DBClusterParameterGroupName":{"Ref":"Graph"}}}}}`
	if _, err := c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: new("cluster-parameters"), TemplateBody: new(template),
	}); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "cluster-parameters", cfntypes.StackStatusCreateComplete)
	n := neptune.New(neptune.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	clusters, err := n.DescribeDBClusters(ctx, &neptune.DescribeDBClustersInput{DBClusterIdentifier: new("graph")})
	if err != nil || len(clusters.DBClusters) != 1 {
		t.Fatalf("clusters: %+v %v", clusters, err)
	}

	group := clusters.DBClusters[0].DBClusterParameterGroup
	out, err := n.DescribeDBClusterParameters(ctx, &neptune.DescribeDBClusterParametersInput{
		DBClusterParameterGroupName: group, Source: new("user"),
	})
	if err != nil || len(out.Parameters) != 1 || *out.Parameters[0].ParameterValue != "5000" {
		t.Fatalf("parameters of %v: %+v %v", group, out, err)
	}

	// Removing the property returns the cluster to its family's default group.
	updated := strings.Replace(template, `,"DBClusterParameterGroupName":{"Ref":"Graph"}`, "", 1)
	if _, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName: new("cluster-parameters"), TemplateBody: new(updated),
	}); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "cluster-parameters", cfntypes.StackStatusUpdateComplete)
	clusters, err = n.DescribeDBClusters(ctx, &neptune.DescribeDBClustersInput{DBClusterIdentifier: new("graph")})
	if err != nil || *clusters.DBClusters[0].DBClusterParameterGroup != "default.neptune1.3" {
		t.Fatalf("clusters: %+v %v", clusters, err)
	}
}
