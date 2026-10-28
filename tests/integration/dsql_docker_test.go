package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/dsql/auth"
	sdk "github.com/aws/aws-sdk-go-v2/service/dsql"
	"github.com/aws/aws-sdk-go-v2/service/dsql/types"
	_ "github.com/lib/pq"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/gateway"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func TestDSQLDocker(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dir := t.TempDir()
	store, setupErr := sqlite.Open(ctx, dir)
	if setupErr != nil {
		t.Fatal(setupErr)
	}

	defer func() { _ = store.Close() }()
	cfg := testConfig()
	cfg.Storage.Path = dir
	cfg.Storage.Mode = "persistent"
	cfg.DSQL.Clusters = config.ContainersDocker
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.SecretKeys = map[string]string{"test": "test"}
	cfg.Identity.Authorization = config.AuthorizationEnforce
	s, stop := serverStore(t, cfg, &fakeRuntime{}, store)
	t.Cleanup(func() { stop() })
	c := dsqlClient(s, "us-east-1")
	cluster, setupErr := c.CreateCluster(
		ctx,
		&sdk.CreateClusterInput{
			DeletionProtectionEnabled: new(false),
			MultiRegionProperties:     &types.MultiRegionProperties{WitnessRegion: new("us-west-2")},
		},
	)
	if setupErr != nil {
		t.Fatal(setupErr)
	}

	peerClient := dsqlClient(s, "us-east-2")
	peer, setupErr := peerClient.CreateCluster(ctx, &sdk.CreateClusterInput{
		DeletionProtectionEnabled: new(false),
		MultiRegionProperties: &types.MultiRegionProperties{
			WitnessRegion: new("us-west-2"),
			Clusters:      []string{*cluster.Arn},
		},
	})
	if setupErr != nil {
		t.Fatal(setupErr)
	}

	pollDSQL(t, c, cluster.Identifier, types.ClusterStatusPendingSetup)
	if _, err := c.UpdateCluster(ctx, &sdk.UpdateClusterInput{
		Identifier:            cluster.Identifier,
		MultiRegionProperties: &types.MultiRegionProperties{Clusters: []string{*peer.Arn}},
	}); err != nil {
		t.Fatal(err)
	}

	// PostgreSQL image download and initialization can exceed the records-mode polling deadline.
	for {
		live, err := c.GetCluster(ctx, &sdk.GetClusterInput{Identifier: cluster.Identifier})
		if err != nil {
			t.Fatal(err)
		}

		if live.Status == types.ClusterStatusActive {
			break
		}

		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}

	address := func() string {
		t.Helper()
		response, err := http.Get(s.URL + "/_tinystack/resources")
		if err != nil {
			t.Fatal(err)
		}

		defer response.Body.Close()
		var out struct {
			Resources []catalog.Owned `json:"resources"`
		}

		if err := json.NewDecoder(response.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}

		for _, resource := range out.Resources {
			if resource.ARN == *cluster.Arn {
				return fmt.Sprint(resource.Details.(map[string]any)["Address"])
			}
		}

		t.Fatal("missing DSQL resource")
		return ""
	}

	token, setupErr := auth.GenerateDBConnectAdminAuthToken(ctx, *cluster.Endpoint, "us-east-1", creds("test"))
	if setupErr != nil {
		t.Fatal(setupErr)
	}

	open := func(user, password string) *sql.DB {
		t.Helper()
		var host string

		if _, err := fmt.Sscanf(address(), "%s", &host); err != nil {
			t.Fatal(err)
		}

		db, err := sql.Open(
			"postgres",
			fmt.Sprintf(
				"postgres://%s:%s@%s/postgres?sslmode=require",
				url.QueryEscape(user),
				url.QueryEscape(password),
				host,
			),
		)
		if err != nil {
			t.Fatal(err)
		}

		return db
	}

	db := open("admin", token)
	defer db.Close()
	if _, err := db.ExecContext(
		ctx,
		"CREATE TABLE durable (value integer); INSERT INTO durable VALUES (42)",
	); err != nil {
		t.Fatal(err)
	}

	pollDSQL(t, peerClient, peer.Identifier, types.ClusterStatusActive)
	original := cluster
	cluster = peer
	peerToken, setupErr := auth.GenerateDBConnectAdminAuthToken(ctx, *peer.Endpoint, "us-east-2", creds("test"))
	if setupErr != nil {
		t.Fatal(setupErr)
	}

	peerDB := open("admin", peerToken)
	var peerValue int
	if err := peerDB.QueryRowContext(ctx, "SELECT value FROM durable").Scan(&peerValue); err != nil || peerValue != 42 {
		t.Fatal(peerValue, err)
	}

	_ = peerDB.Close()
	cluster = original
	var owner string
	if err := store.View(ctx, func(tx storage.ReadTx) error {
		stored, _, err := storage.Read[string](tx, "aws/dsql/runtime-owner")
		if err == nil {
			owner = *stored
		}

		return err
	}); err != nil {
		t.Fatal(err)
	}

	containers, _, setupErr := dockercli.ListOwned(ctx, "tinystack.dsql", "tinystack.dsql.owner", owner)
	if setupErr != nil || len(containers) != 1 {
		t.Fatal("peers must share one backend", containers, setupErr)
	}

	for id := range containers {
		port, err := dockercli.HostPort(ctx, id, 5432)
		if err != nil {
			t.Fatal(err)
		}

		direct, err := sql.Open(
			"postgres",
			fmt.Sprintf("postgres://admin:known@127.0.0.1:%d/postgres?sslmode=require", port),
		)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := db.ExecContext(ctx, "ALTER ROLE admin PASSWORD 'known'"); err != nil {
			t.Fatal(err)
		}

		if err := direct.PingContext(ctx); err == nil {
			t.Fatal("backend accepted password without a proxy client certificate")
		}

		_ = direct.Close()
	}

	var value int
	if err := db.QueryRowContext(ctx, "SELECT value FROM durable").Scan(&value); err != nil || value != 42 {
		t.Fatal(value, err)
	}

	if _, err := db.ExecContext(ctx, "CREATE ROLE reader WITH LOGIN; GRANT SELECT ON durable TO reader"); err != nil {
		t.Fatal(err)
	}

	userToken, setupErr := auth.GenerateDbConnectAuthToken(ctx, *cluster.Endpoint, "us-east-1", creds("test"))
	if setupErr != nil {
		t.Fatal(setupErr)
	}

	reader := open("reader", userToken)
	defer reader.Close()
	if err := reader.QueryRowContext(ctx, "SELECT value FROM durable").Scan(&value); err != nil || value != 42 {
		t.Fatal(value, err)
	}

	bad := open("admin", "bad")
	defer bad.Close()
	if err := bad.PingContext(ctx); err == nil {
		t.Fatal("accepted bad token")
	}

	if _, err := reader.ExecContext(ctx, "SET ROLE admin"); err == nil {
		t.Fatal("non-admin assumed admin role")
	}

	tampered := open("admin", strings.Replace(token, "X-Amz-Signature=", "X-Amz-Signature=0", 1))
	defer tampered.Close()
	if err := tampered.PingContext(ctx); err == nil {
		t.Fatal("accepted invalid signature")
	}

	expiredToken, setupErr := auth.GenerateDBConnectAdminAuthToken(ctx, *cluster.Endpoint, "us-east-1", creds("test"),
		func(options *auth.TokenOptions) { options.ExpiresIn = -time.Second })
	if setupErr != nil {
		t.Fatal(setupErr)
	}

	expired := open("admin", expiredToken)
	defer expired.Close()
	if err := expired.PingContext(ctx); err == nil {
		t.Fatal("accepted expired token")
	}

	issued := authorizationUser(t, s)
	deniedToken, setupErr := auth.GenerateDbConnectAuthToken(
		ctx,
		*cluster.Endpoint,
		"us-east-1",
		authorizationProvider(issued),
	)
	if setupErr != nil {
		t.Fatal(setupErr)
	}

	denied := open("reader", deniedToken)
	defer denied.Close()
	if err := denied.PingContext(ctx); err == nil {
		t.Fatal("IAM enforcement accepted a principal without DbConnect permission")
	}

	response, setupErr := http.Get(s.URL + "/_tinystack/requests")
	if setupErr != nil {
		t.Fatal(setupErr)
	}

	var inspection struct {
		Requests []gateway.Record `json:"requests"`
	}

	setupErr = json.NewDecoder(response.Body).Decode(&inspection)
	_ = response.Body.Close()
	if setupErr != nil {
		t.Fatal(setupErr)
	}

	foundDenial := false
	for _, entry := range inspection.Requests {
		if entry.Service == "dsql" && entry.Protocol == "postgres" && entry.Operation == "DbConnect" &&
			entry.Authorization != nil && !entry.Authorization.Allowed {
			foundDenial = true
		}
	}

	if !foundDenial {
		t.Fatal("missing database IAM denial in request inspector")
	}

	if _, err := db.ExecContext(ctx, "AWS IAM REVOKE reader FROM 'arn:aws:iam::000000000000:role/reader'"); err == nil {
		t.Fatal("accepted unsupported AWS IAM REVOKE")
	}

	if _, err := db.ExecContext(ctx, "AWS IAM GRANT reader TO 'arn:aws:iam::000000000000:role/reader'"); err == nil {
		t.Fatal("accepted unsupported AWS IAM statement")
	}

	_ = db.Close()
	_ = reader.Close()
	stop()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, setupErr = sqlite.Open(ctx, dir)
	if setupErr != nil {
		t.Fatal(setupErr)
	}

	s, stop = serverStore(t, cfg, &fakeRuntime{}, store)
	c = dsqlClient(s, "us-east-1")
	peerClient = dsqlClient(s, "us-east-2")
	pollDSQL(t, peerClient, peer.Identifier, types.ClusterStatusActive)
	pollDSQL(t, c, cluster.Identifier, types.ClusterStatusActive)
	db = open("admin", token)
	defer db.Close()
	if err := db.QueryRowContext(ctx, "SELECT value FROM durable").Scan(&value); err != nil || value != 42 {
		t.Fatal(value, err)
	}

	if _, err := c.DeleteCluster(ctx, &sdk.DeleteClusterInput{Identifier: cluster.Identifier}); err != nil {
		t.Fatal(err)
	}

	peerClient = dsqlClient(s, "us-east-2")
	if _, err := peerClient.DeleteCluster(ctx, &sdk.DeleteClusterInput{Identifier: peer.Identifier}); err != nil {
		t.Fatal(err)
	}

	pollDSQL(t, c, cluster.Identifier, "")
	pollDSQL(t, peerClient, peer.Identifier, "")
	containers, volumes, setupErr := dockercli.ListOwned(ctx, "tinystack.dsql", "tinystack.dsql.owner", owner)
	if setupErr != nil || len(containers) != 0 || len(volumes) != 0 {
		t.Fatal("delete leaked backend resources", containers, volumes, setupErr)
	}

	fresh, setupErr := c.CreateCluster(ctx, &sdk.CreateClusterInput{DeletionProtectionEnabled: new(false)})
	if setupErr != nil {
		t.Fatal(setupErr)
	}

	pollDSQL(t, c, fresh.Identifier, types.ClusterStatusActive)
	reset, setupErr := http.Post(s.URL+"/_tinystack/reset", "application/json", nil)
	if setupErr != nil {
		t.Fatal(setupErr)
	}

	_ = reset.Body.Close()
	if reset.StatusCode != http.StatusOK {
		t.Fatal("reset failed", reset.StatusCode)
	}

	containers, volumes, setupErr = dockercli.ListOwned(ctx, "tinystack.dsql", "tinystack.dsql.owner", owner)
	if setupErr != nil || len(containers) != 0 || len(volumes) != 0 {
		t.Fatal("reset leaked backend resources", containers, volumes, setupErr)
	}

	fresh, setupErr = c.CreateCluster(ctx, &sdk.CreateClusterInput{DeletionProtectionEnabled: new(false)})
	if setupErr != nil {
		t.Fatal(setupErr)
	}

	pollDSQL(t, c, fresh.Identifier, types.ClusterStatusActive)
	if _, err := c.DeleteCluster(ctx, &sdk.DeleteClusterInput{Identifier: fresh.Identifier}); err != nil {
		t.Fatal(err)
	}

	pollDSQL(t, c, fresh.Identifier, "")
}
