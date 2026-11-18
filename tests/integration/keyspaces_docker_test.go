package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/service/keyspaces"
	"github.com/aws/aws-sdk-go-v2/service/keyspaces/types"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func TestKeyspacesDocker(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1 for live Cassandra")
	}

	checkCtx, checkCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := dockercli.Run(checkCtx, nil, "info")
	checkCancel()
	if err != nil {
		t.Skipf("Docker daemon unavailable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dir := t.TempDir()
	store, err := sqlite.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = store.Close() }()
	cfg := testConfig()
	cfg.Storage.Mode, cfg.Storage.Path = config.StoragePersistent, dir
	cfg.Keyspaces.Runtime = config.ContainersDocker
	s, stop := serverStore(t, cfg, &fakeRuntime{}, store)
	defer func() {
		cleanup, finish := context.WithTimeout(context.Background(), 30*time.Second)
		defer finish()
		_ = keyspacesReset(cleanup, s)
		stop()
	}()
	account := fmt.Sprintf("%012d", time.Now().UnixNano()%1_000_000_000_000)
	const region, ks, table = "us-east-1", "docker_test", "events"
	filter := "label=tinystack.keyspaces.scope=" + account + "/" + region
	c := keyspacesClient(s, account, region)
	if _, err = c.CreateKeyspace(ctx, &sdk.CreateKeyspaceInput{KeyspaceName: new(ks)}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.UpdateKeyspace(ctx, &sdk.UpdateKeyspaceInput{
		KeyspaceName: new(ks), ReplicationSpecification: &types.ReplicationSpecification{
			ReplicationStrategy: types.RsMultiRegion, RegionList: []string{region, "us-west-2"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreateType(ctx, &sdk.CreateTypeInput{
		KeyspaceName: new(ks), TypeName: new("address"),
		FieldDefinitions: []types.FieldDefinition{{Name: new("city"), Type: new("text")}},
	}); err != nil {
		t.Fatal(err)
	}

	in := keyspacesTable(ks, table)
	in.SchemaDefinition.AllColumns = append(in.SchemaDefinition.AllColumns,
		types.ColumnDefinition{Name: new("location"), Type: new("frozen<address>")})
	if _, err = c.CreateTable(ctx, in); err != nil {
		t.Fatal(err)
	}

	awaitKeyspacesTable(ctx, t, c, ks, table)
	awaitKeyspacesEndpoint(ctx, t, s, account)
	container := func() string {
		t.Helper()
		id, e := dockercli.Run(ctx, nil, "ps", "-q", "--filter", filter)
		if e != nil || len(strings.Fields(id)) != 1 {
			t.Fatalf("expected one Cassandra container for the scope: %q %v", id, e)
		}

		return id
	}
	cql := func(statement string) string {
		t.Helper()
		out, e := dockercli.Run(ctx, nil, "exec", container(), "cqlsh", "-e", statement)
		if e != nil {
			t.Fatal(e)
		}

		return out
	}
	cql("INSERT INTO docker_test.events (id, n, location) VALUES ('persisted', 7, {city: 'Oslo'});")
	_, err = c.UpdateTable(ctx, &sdk.UpdateTableInput{
		KeyspaceName: new(ks), TableName: new(table), DefaultTimeToLive: new(int32(60)),
		AddColumns: []types.ColumnDefinition{{Name: new("extra"), Type: new("text")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	awaitKeyspacesTable(ctx, t, c, ks, table)
	if out := cql("SELECT extra FROM docker_test.events WHERE id = 'persisted';"); !strings.Contains(out, "extra") {
		t.Fatalf("expected the added column: %s", out)
	}

	previous := container()
	stop()
	s, stop = serverStore(t, cfg, &fakeRuntime{}, store)
	c = keyspacesClient(s, account, region)
	awaitKeyspacesEndpoint(ctx, t, s, account)
	if id := container(); id == previous {
		t.Fatal("expected shutdown to remove the previous container")
	}

	if out := cql("SELECT location FROM docker_test.events WHERE id = 'persisted';"); !strings.Contains(out, "Oslo") {
		t.Fatalf("Cassandra row after restart: %s", out)
	}

	if _, err = c.DeleteTable(ctx, &sdk.DeleteTableInput{KeyspaceName: new(ks), TableName: new(table)}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteType(ctx, &sdk.DeleteTypeInput{KeyspaceName: new(ks), TypeName: new("address")}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteKeyspace(ctx, &sdk.DeleteKeyspaceInput{KeyspaceName: new(ks)}); err != nil {
		t.Fatal(err)
	}

	// Creating a second keyspace after the deletes provides a read-after-apply marker for the scope's queue.
	if _, err = c.CreateKeyspace(ctx, &sdk.CreateKeyspaceInput{KeyspaceName: new("marker")}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreateTable(ctx, keyspacesTable("marker", "marker_table")); err != nil {
		t.Fatal(err)
	}

	awaitKeyspacesTable(ctx, t, c, "marker", "marker_table")
	query := "SELECT keyspace_name FROM system_schema.keyspaces WHERE keyspace_name = 'docker_test';"
	if out := cql(query); !strings.Contains(out, "(0 rows)") {
		t.Fatalf("deleted keyspace still exists in Cassandra: %s", out)
	}

	if err = keyspacesReset(ctx, s); err != nil {
		t.Fatal(err)
	}

	for _, command := range [][]string{{"ps", "-aq", "--filter", filter}, {"volume", "ls", "-q", "--filter", filter}} {
		if out, e := dockercli.Run(ctx, nil, command...); e != nil || out != "" {
			t.Fatalf("owned Docker resources after reset: %q %v", out, e)
		}
	}
}

func awaitKeyspacesEndpoint(ctx context.Context, t *testing.T, s *httptest.Server, account string) {
	t.Helper()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		endpoint, err := keyspacesEndpoint(ctx, s, account)
		if err != nil {
			t.Fatal(err)
		}

		if endpoint != "" {
			host, _, e := net.SplitHostPort(endpoint)
			if e != nil || host != "127.0.0.1" {
				t.Fatalf("CQL endpoint %q: %v", endpoint, e)
			}

			conn, e := new(net.Dialer).DialContext(ctx, "tcp", endpoint)
			if e != nil {
				t.Fatal(e)
			}

			_ = conn.Close()
			return
		}

		select {
		case <-ctx.Done():
			t.Fatalf("no published CQL endpoint: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func keyspacesEndpoint(ctx context.Context, s *httptest.Server, account string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+"/_tinystack/resources", http.NoBody)
	if err != nil {
		return "", err
	}

	resp, err := s.Client().Do(req)
	if err != nil {
		return "", err
	}

	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resources endpoint returned %s", resp.Status)
	}

	var inventory struct{ Resources []catalog.Owned }
	if err := json.NewDecoder(resp.Body).Decode(&inventory); err != nil {
		return "", err
	}

	for _, item := range inventory.Resources {
		if item.Service != "keyspaces" || item.Account != account || item.Type != "keyspace" {
			continue
		}

		details, _ := item.Details.(map[string]any)
		if endpoint, ok := details["CqlEndpoint"].(string); ok {
			return endpoint, nil
		}
	}

	return "", nil
}

func keyspacesReset(ctx context.Context, s *httptest.Server) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/_tinystack/reset", http.NoBody)
	if err != nil {
		return err
	}

	resp, err := s.Client().Do(req)
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("reset returned %s", resp.Status)
	}

	return nil
}
