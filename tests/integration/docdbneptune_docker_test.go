package integration_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/docdb"
	"github.com/aws/aws-sdk-go-v2/service/rds"

	rdsapi "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/gateway"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	rdssvc "github.com/amjadjibon/tinystack/internal/services/rds"
	rdsdocker "github.com/amjadjibon/tinystack/internal/services/rds/docker"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func clusterDockerServer(t *testing.T, store storage.Store) (*httptest.Server, func()) {
	t.Helper()
	service := rdssvc.New(store)
	if err := service.UseRuntime(isolatedRDSRuntime{rdsdocker.New()}); err != nil {
		t.Fatal(err)
	}

	h, err := gateway.New(testConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)),
		rdsapi.Descriptor(service))
	if err != nil {
		t.Fatal(err)
	}

	s := httptest.NewServer(h)
	return s, func() { service.Stop(); s.Close() }
}

func TestRDSDockerDocumentDB(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1 for live DocumentDB")
	}

	checkCtx, checkCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := dockercli.Run(checkCtx, nil, "info")
	checkCancel()
	if err != nil {
		t.Skipf("Docker daemon unavailable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	store, err := sqlite.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer store.Close()
	s, stop := clusterDockerServer(t, store)
	defer func() { stop() }()
	c := docdb.New(docdb.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	id := fmt.Sprintf("documents-%d", time.Now().UnixNano())
	member := id + "-member"
	password := "initial-password"
	var spec *rdssvc.Spec
	defer func() {
		if spec != nil {
			_ = rdsdocker.New().Remove(context.Background(), spec, true)
		}
	}()
	out, err := c.CreateDBCluster(ctx, &docdb.CreateDBClusterInput{
		DBClusterIdentifier: new(
			id,
		),
		Engine:             new("docdb"),
		MasterUsername:     new("master"),
		MasterUserPassword: new(password),
	})
	if err != nil || aws.ToString(out.DBCluster.Status) != "creating" || out.DBCluster.Endpoint != nil {
		t.Fatal(out, err)
	}

	_, err = c.CreateDBInstance(ctx, &docdb.CreateDBInstanceInput{
		DBInstanceIdentifier: new(
			member,
		),
		DBClusterIdentifier: new(id),
		DBInstanceClass:     new("db.r5.large"),
		Engine:              new("docdb"),
	})
	if err != nil {
		t.Fatal(err)
	}

	r := rdsClient(s, "test", "us-east-1")
	cluster := waitAurora(t, ctx, r, id)
	// Identify only this test's container; discovery must never clean other runtimes.
	container, err := clusterContainer(ctx, aws.ToString(cluster.DBClusterArn))
	if err != nil || container == "" {
		t.Fatal(container, err)
	}

	{
		name := container
		volume, inspectErr := dockercli.Run(
			ctx,
			nil,
			"inspect",
			"--format",
			"{{range .Mounts}}{{if eq .Destination \"/data/db\"}}{{.Name}}{{end}}{{end}}",
			name,
		)
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}

		spec = &rdssvc.Spec{Name: name, Volume: strings.TrimSpace(volume)}
	}

	query := func(script string) {
		t.Helper()
		_, queryErr := dockercli.Run(ctx, nil, "exec", spec.Name, "mongosh", "--quiet", "--username", "master",
			"--password", password, "--authenticationDatabase", "admin", "test", "--eval", script)
		if queryErr != nil {
			t.Fatal(queryErr)
		}
	}
	query(
		"db.documents.insertOne({_id:1,value:'durable'}); if(db.documents.findOne({_id:1}).value !== 'durable') quit(1)",
	)
	_, err = c.ModifyDBCluster(
		ctx,
		&docdb.ModifyDBClusterInput{DBClusterIdentifier: new(id), MasterUserPassword: new("changed-password")},
	)
	if err != nil {
		t.Fatal(err)
	}

	password = "changed-password"
	query("if(db.documents.countDocuments({_id:1}) !== 1) quit(1)")
	_, err = c.StopDBCluster(ctx, &docdb.StopDBClusterInput{DBClusterIdentifier: new(id)})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.StartDBCluster(ctx, &docdb.StartDBClusterInput{DBClusterIdentifier: new(id)})
	if err != nil {
		t.Fatal(err)
	}

	waitAurora(t, ctx, r, id)
	query("if(db.documents.countDocuments({_id:1}) !== 1) quit(1)")
	stop()
	s, stop = clusterDockerServer(t, store)
	r = rdsClient(s, "test", "us-east-1")
	cluster = waitAurora(t, ctx, r, id)
	container, err = clusterContainer(ctx, aws.ToString(cluster.DBClusterArn))
	if err != nil || container == "" {
		t.Fatal(container, err)
	}

	spec.Name = container

	query("if(db.documents.findOne({_id:1}).value !== 'durable') quit(1)")
	c = docdb.New(docdb.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	_, err = c.DeleteDBInstance(ctx, &docdb.DeleteDBInstanceInput{DBInstanceIdentifier: new(member)})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteDBCluster(
		ctx,
		&docdb.DeleteDBClusterInput{DBClusterIdentifier: new(id), SkipFinalSnapshot: new(true)},
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, resource := range [][]string{{"inspect", spec.Name}, {"volume", "inspect", spec.Volume}} {
		if _, err := dockercli.Run(ctx, nil, resource...); err == nil || !dockercli.Gone(err) {
			t.Fatal("resource survived delete", resource, err)
		}
	}

	_, err = r.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{DBClusterIdentifier: new(id)})
	code(t, err, "DBClusterNotFoundFault")
}

func clusterContainer(ctx context.Context, arn string) (string, error) {
	out, err := dockercli.Run(ctx, nil, "ps", "-a", "--filter", "label=tinystack.rds="+arn, "--format", "{{.ID}}")
	return strings.TrimSpace(out), err
}
