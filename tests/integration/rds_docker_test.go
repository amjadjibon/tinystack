package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
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

	wait := func(instanceID string) *rds.DescribeDBInstancesOutput {
		t.Helper()
		for ctx.Err() == nil {
			out, e := c.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{DBInstanceIdentifier: new(instanceID)})
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

	db := connect(wait(id))
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

	snapshotID, copyID, restoredID, finalID := id+"-snapshot", id+"-copy", id+"-restored", id+"-final"
	t.Cleanup(func() {
		_, _ = c.DeleteDBInstance(context.Background(), &rds.DeleteDBInstanceInput{
			DBInstanceIdentifier: new(restoredID), SkipFinalSnapshot: new(true),
		})
		for _, snapshotID := range []string{snapshotID, copyID, finalID} {
			_, _ = c.DeleteDBSnapshot(
				context.Background(),
				&rds.DeleteDBSnapshotInput{DBSnapshotIdentifier: new(snapshotID)},
			)
		}
	})
	if _, err = c.CreateDBSnapshot(ctx, &rds.CreateDBSnapshotInput{
		DBInstanceIdentifier: new(id), DBSnapshotIdentifier: new(snapshotID),
	}); err != nil {
		t.Fatal(err)
	}

	waitRDSSnapshot(t, c, snapshotID)
	// The source is still running, and subsequent writes must not change either snapshot copy.
	if _, err = db.ExecContext(ctx, "UPDATE durable SET value = 99"); err != nil {
		t.Fatal(err)
	}

	if _, err = c.CopyDBSnapshot(ctx, &rds.CopyDBSnapshotInput{
		SourceDBSnapshotIdentifier: new(snapshotID), TargetDBSnapshotIdentifier: new(copyID),
	}); err != nil {
		t.Fatal(err)
	}

	copySnapshot := waitRDSSnapshot(t, c, copyID)
	if _, err = c.DeleteDBSnapshot(ctx, &rds.DeleteDBSnapshotInput{DBSnapshotIdentifier: new(snapshotID)}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.RestoreDBInstanceFromDBSnapshot(ctx, &rds.RestoreDBInstanceFromDBSnapshotInput{
		DBInstanceIdentifier: new(restoredID), DBSnapshotIdentifier: new(copyID),
	}); err != nil {
		t.Fatal(err)
	}

	restoredDB := connect(wait(restoredID))
	if err = restoredDB.QueryRowContext(ctx, "SELECT value FROM durable").Scan(&one); err != nil || one != 42 {
		t.Fatalf("snapshot restore data: %d %v", one, err)
	}

	restoredDB.Close()
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
	db = connect(wait(id))
	if err = db.QueryRowContext(ctx, "SELECT value FROM durable").Scan(&one); err != nil || one != 99 {
		t.Fatalf("durable data: %d %v", one, err)
	}

	db.Close()
	if _, err = c.DeleteDBInstance(
		ctx,
		&rds.DeleteDBInstanceInput{DBInstanceIdentifier: new(id), FinalDBSnapshotIdentifier: new(finalID)},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = dockercli.Run(ctx, nil, "inspect", containers); !dockercli.Gone(err) {
		t.Fatalf("container remains: %v", err)
	}

	if _, err = dockercli.Run(ctx, nil, "volume", "inspect", volume); !dockercli.Gone(err) {
		t.Fatalf("volume remains: %v", err)
	}

	finalSnapshot := waitRDSSnapshot(t, c, finalID)
	snapshotVolumes := make([]string, 0, 2)
	snapshotARNs := []string{aws.ToString(copySnapshot.DBSnapshotArn), aws.ToString(finalSnapshot.DBSnapshotArn)}
	for _, snapshot := range snapshotARNs {
		volumes, e := dockercli.Run(ctx, nil, "volume", "ls", "--filter", "label=tinystack.rds="+snapshot,
			"--format", "{{.Name}}")
		if e != nil || volumes == "" {
			t.Fatalf("snapshot volume: %q %v", volumes, e)
		}

		snapshotVolumes = append(snapshotVolumes, volumes)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/_tinystack/reset", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	response, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("reset status: %d", response.StatusCode)
	}

	for _, volume := range snapshotVolumes {
		if _, err = dockercli.Run(ctx, nil, "volume", "inspect", volume); !dockercli.Gone(err) {
			t.Fatalf("snapshot volume remains after reset: %s %v", volume, err)
		}
	}
}
