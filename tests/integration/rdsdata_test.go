package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/rdsdata"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

func rdsDataClient(s *httptest.Server) *rdsdata.Client {
	return rdsdata.New(rdsdata.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL), Retryer: aws.NopRetryer{},
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
	})
}

func auroraInput(id, engine string) *rds.CreateDBClusterInput {
	return &rds.CreateDBClusterInput{
		DBClusterIdentifier: new(id),
		Engine:              new(engine),
		MasterUsername:      new("testuser"),
		MasterUserPassword:  new("password123"),
		DatabaseName:        new("testdb"),
		EnableHttpEndpoint:  new(true),
		ServerlessV2ScalingConfiguration: &rdstypes.ServerlessV2ScalingConfiguration{
			MinCapacity: new(0.5),
			MaxCapacity: new(4.0),
		},
	}
}

func waitAurora(t *testing.T, ctx context.Context, c *rds.Client, id string) *rdstypes.DBCluster {
	t.Helper()
	for ctx.Err() == nil {
		out, err := c.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{DBClusterIdentifier: new(id)})
		if err != nil {
			t.Fatal(err)
		}

		cluster := out.DBClusters[0]
		switch aws.ToString(cluster.Status) {
		case "available":
			return &cluster
		case "failed":
			t.Fatal("cluster startup failed")
		}

		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}

	t.Fatal(ctx.Err())
	return nil
}

func TestAuroraControlPlane(t *testing.T) {
	s := server(t)
	c := rdsClient(s, "test", "us-east-1")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, engine := range []string{"aurora-postgresql", "aurora-mysql"} {
		t.Run(engine, func(t *testing.T) {
			id := "cluster-" + engine
			in := auroraInput(id, engine)
			in.MasterUserPassword = nil
			in.ManageMasterUserPassword = new(true)
			out, err := c.CreateDBCluster(ctx, in)
			if err != nil {
				t.Fatal(err)
			}

			if aws.ToString(out.DBCluster.Status) != "creating" || out.DBCluster.MasterUserSecret == nil ||
				aws.ToString(out.DBCluster.MasterUserSecret.SecretStatus) != "active" {
				t.Fatalf("cluster: %+v", out.DBCluster)
			}

			cluster := waitAurora(t, ctx, c, id)
			expectedVersion, expectedPort := "17.7", int32(5432)
			if engine == "aurora-mysql" {
				expectedVersion, expectedPort = "8.0.mysql_aurora.3.12.0", 3306
			}

			if aws.ToString(cluster.EngineVersion) != expectedVersion || aws.ToInt32(cluster.Port) != expectedPort {
				t.Fatal(cluster)
			}

			if !aws.ToBool(cluster.HttpEndpointEnabled) || aws.ToString(cluster.DatabaseName) != "testdb" ||
				cluster.ServerlessV2ScalingConfiguration == nil {
				t.Fatalf("cluster: %+v", cluster)
			}

			var secret struct{ Username, Password string }
			secretOutput, secretErr := secretsClient(s, "test", "us-east-1").GetSecretValue(ctx,
				&secretsmanager.GetSecretValueInput{SecretId: cluster.MasterUserSecret.SecretArn})
			if secretErr != nil {
				t.Fatal(secretErr)
			}

			value := aws.ToString(secretOutput.SecretString)
			if err = json.Unmarshal(
				[]byte(value),
				&secret,
			); err != nil || secret.Username != "testuser" ||
				len(secret.Password) < 8 {
				t.Fatal(value, err)
			}

			member, err := c.CreateDBInstance(
				ctx,
				&rds.CreateDBInstanceInput{
					DBInstanceIdentifier: new(id + "-member"),
					DBClusterIdentifier:  new(id),
					Engine:               new(engine),
					DBInstanceClass:      new("db.serverless"),
				},
			)
			if err != nil {
				t.Fatal(err)
			}

			if aws.ToString(member.DBInstance.Endpoint.Address) != aws.ToString(cluster.Endpoint) {
				t.Fatal(member.DBInstance)
			}

			_, err = c.DisableHttpEndpoint(ctx, &rds.DisableHttpEndpointInput{ResourceArn: cluster.DBClusterArn})
			if err != nil {
				t.Fatal(err)
			}

			_, err = c.EnableHttpEndpoint(ctx, &rds.EnableHttpEndpointInput{ResourceArn: cluster.DBClusterArn})
			if err != nil {
				t.Fatal(err)
			}

			_, err = c.StopDBCluster(ctx, &rds.StopDBClusterInput{DBClusterIdentifier: new(id)})
			if err != nil {
				t.Fatal(err)
			}

			_, err = c.StartDBCluster(ctx, &rds.StartDBClusterInput{DBClusterIdentifier: new(id)})
			if err != nil {
				t.Fatal(err)
			}

			waitAurora(t, ctx, c, id)
			_, err = c.ModifyDBCluster(
				ctx,
				&rds.ModifyDBClusterInput{
					DBClusterIdentifier: new(id),
					DeletionProtection:  new(true),
					ApplyImmediately:    new(true),
				},
			)
			if err != nil {
				t.Fatal(err)
			}

			_, err = c.DeleteDBCluster(
				ctx,
				&rds.DeleteDBClusterInput{DBClusterIdentifier: new(id), SkipFinalSnapshot: new(true)},
			)
			code(t, err, "InvalidDBClusterStateFault")
			_, err = c.ModifyDBCluster(
				ctx,
				&rds.ModifyDBClusterInput{DBClusterIdentifier: new(id), DeletionProtection: new(false)},
			)
			if err != nil {
				t.Fatal(err)
			}

			_, err = c.DeleteDBInstance(
				ctx,
				&rds.DeleteDBInstanceInput{DBInstanceIdentifier: new(id + "-member"), SkipFinalSnapshot: new(true)},
			)
			if err != nil {
				t.Fatal(err)
			}

			_, err = c.DeleteDBCluster(
				ctx,
				&rds.DeleteDBClusterInput{DBClusterIdentifier: new(id), FinalDBSnapshotIdentifier: new(id + "-final")},
			)
			if err != nil {
				t.Fatal(err)
			}

			snapshots, err := c.DescribeDBClusterSnapshots(
				ctx,
				&rds.DescribeDBClusterSnapshotsInput{DBClusterIdentifier: new(id)},
			)
			if err != nil || len(snapshots.DBClusterSnapshots) != 1 {
				t.Fatal(snapshots, err)
			}
		})
	}

	for i, mutate := range []func(*rds.CreateDBClusterInput){
		func(in *rds.CreateDBClusterInput) { in.EngineVersion = new("99.1") },
		func(in *rds.CreateDBClusterInput) { in.MasterUsername = new("bad/name") },
		func(in *rds.CreateDBClusterInput) { in.DatabaseName = new("bad-name") },
		func(in *rds.CreateDBClusterInput) { in.ManageMasterUserPassword = new(true) },
		func(in *rds.CreateDBClusterInput) { in.ServerlessV2ScalingConfiguration.MinCapacity = new(0.3) },
	} {
		in := auroraInput(fmt.Sprintf("invalid-%d", i), "aurora-postgresql")
		mutate(in)
		_, err := c.CreateDBCluster(ctx, in)
		code(t, err, "InvalidParameterValue")
	}
}

func TestRDSDataWithoutDocker(t *testing.T) {
	s := server(t)
	c := rdsClient(s, "test", "us-east-1")
	data := rdsDataClient(s)
	ctx := context.Background()
	in := auroraInput("data-cluster", "aurora-postgresql")
	out, err := c.CreateDBCluster(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	secret, err := secretsClient(
		s,
		"test",
		"us-east-1",
	).CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name: new("data-secret"), SecretString: new(`{"username":"testuser","password":"password123"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	execute := &rdsdata.ExecuteStatementInput{
		ResourceArn: out.DBCluster.DBClusterArn,
		SecretArn:   secret.ARN,
		Sql:         new("SELECT 1"),
	}

	_, err = data.ExecuteStatement(ctx, execute)
	code(t, err, "InvalidResourceStateException")
	_, err = data.BeginTransaction(
		ctx,
		&rdsdata.BeginTransactionInput{ResourceArn: execute.ResourceArn, SecretArn: execute.SecretArn},
	)
	code(t, err, "InvalidResourceStateException")
	execute.SecretArn = new("missing")
	_, err = data.ExecuteStatement(ctx, execute)
	code(t, err, "ForbiddenException")
	execute.SecretArn = secret.ARN
	_, err = c.DisableHttpEndpoint(ctx, &rds.DisableHttpEndpointInput{ResourceArn: execute.ResourceArn})
	if err != nil {
		t.Fatal(err)
	}

	_, err = data.ExecuteStatement(ctx, execute)
	code(t, err, "HttpEndpointNotEnabledException")
	execute.ResourceArn = new("arn:aws:rds:us-east-1:000000000000:cluster:missing")
	_, err = data.ExecuteStatement(ctx, execute)
	code(t, err, "NotFoundException")
}
