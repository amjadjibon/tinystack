package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	_ "github.com/lib/pq" // PostgreSQL driver for the opt-in SQL workflow.

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func TestRDSDocker(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1 for live PostgreSQL")
	}

	checkCtx, checkCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := dockercli.Run(checkCtx, nil, "info")
	checkCancel()
	if err != nil {
		t.Skipf("Docker daemon unavailable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store, err := sqlite.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer store.Close()
	cfg := testConfig()
	cfg.RDS.Instances = config.InstancesDocker
	s, stop := serverStore(t, cfg, &fakeRuntime{}, store)
	t.Cleanup(func() { stop() })
	c := rdsClient(s, "test", "us-east-1")
	id := fmt.Sprintf("docker-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = c.DeleteDBInstance(context.Background(), &rds.DeleteDBInstanceInput{
			DBInstanceIdentifier: new(id), SkipFinalSnapshot: new(true),
		})
	})
	create, err := c.CreateDBInstance(ctx, rdsInput(id))
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToString(create.DBInstance.DBInstanceStatus) != "creating" {
		t.Fatal("expected creating")
	}

	wait := func() *rds.DescribeDBInstancesOutput {
		t.Helper()
		for ctx.Err() == nil {
			out, e := c.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{DBInstanceIdentifier: new(id)})
			if e != nil {
				t.Fatal(e)
			}

			if aws.ToString(out.DBInstances[0].DBInstanceStatus) == "failed" {
				t.Fatal("database startup failed")
			}

			if aws.ToString(out.DBInstances[0].DBInstanceStatus) == "available" {
				return out
			}

			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}

		t.Fatal(ctx.Err())
		return nil
	}

	connect := func(out *rds.DescribeDBInstancesOutput) *sql.DB {
		t.Helper()
		endpoint := out.DBInstances[0].Endpoint
		address := url.URL{
			Scheme: "postgres",
			Host:   fmt.Sprintf("%s:%d", aws.ToString(endpoint.Address), aws.ToInt32(endpoint.Port)),
			User: url.UserPassword(
				"testuser",
				"password123",
			),
			Path:     "/testdb",
			RawQuery: "sslmode=disable&connect_timeout=5",
		}

		db, e := sql.Open("postgres", address.String())
		if e != nil {
			t.Fatal(e)
		}

		return db
	}

	db := connect(wait())
	var one int
	if err = db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("SELECT 1: %d %v", one, err)
	}

	if _, err = db.ExecContext(
		ctx,
		"CREATE TABLE durable (value integer); INSERT INTO durable VALUES (42)",
	); err != nil {
		t.Fatal(err)
	}

	db.Close()
	containers, err := dockercli.Run(
		ctx,
		nil,
		"ps",
		"-a",
		"--filter",
		"label=tinystack.rds="+aws.ToString(create.DBInstance.DBInstanceArn),
		"--format",
		"{{.Names}}",
	)
	if err != nil || containers == "" {
		t.Fatalf("container: %q %v", containers, err)
	}

	volume := containers + "-data"
	stop()
	// app.Stop removes the container but leaves the named data volume. A new runtime recreates it.
	if _, err = dockercli.Run(ctx, nil, "volume", "inspect", volume); err != nil {
		t.Fatal(err)
	}

	s, stop = serverStore(t, cfg, &fakeRuntime{}, store)
	c = rdsClient(s, "test", "us-east-1")
	db = connect(wait())
	if err = db.QueryRowContext(ctx, "SELECT value FROM durable").Scan(&one); err != nil || one != 42 {
		t.Fatalf("durable data: %d %v", one, err)
	}

	db.Close()
	if _, err = c.DeleteDBInstance(
		ctx,
		&rds.DeleteDBInstanceInput{DBInstanceIdentifier: new(id), SkipFinalSnapshot: new(true)},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = dockercli.Run(ctx, nil, "inspect", containers); !dockercli.Gone(err) {
		t.Fatalf("container remains: %v", err)
	}

	if _, err = dockercli.Run(ctx, nil, "volume", "inspect", volume); !dockercli.Gone(err) {
		t.Fatalf("volume remains: %v", err)
	}
}
