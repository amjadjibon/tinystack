package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func TestRDSDockerOwnedOrphans(t *testing.T) {
	dockerOwnedOrphans(t, "rds", func(cfg *config.Config) { cfg.RDS.Instances = config.InstancesDocker })
}

func TestOpenSearchDockerOwnedOrphans(t *testing.T) {
	dockerOwnedOrphans(t, "opensearch", func(cfg *config.Config) { cfg.OpenSearch.Domains = config.DomainsDocker })
}

func TestElastiCacheDockerOwnedOrphans(t *testing.T) {
	dockerOwnedOrphans(t, "elasticache", func(cfg *config.Config) { cfg.ElastiCache.Caches = config.CachesDocker })
}

// Exercise real discovery and deletion without starting a database engine.
func dockerOwnedOrphans(t *testing.T, service string, configure func(*config.Config)) {
	t.Helper()
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := sqlite.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer store.Close()
	owner := fmt.Sprintf("%s-owner-%d", service, time.Now().UnixNano())
	err = store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, "aws/"+service+"/runtime-owner", owner)
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = dockercli.Run(ctx, nil, "pull", "alpine:3.21"); err != nil {
		t.Fatal(err)
	}

	names := []string{owner + "-orphan", owner + "-foreign", owner + "-legacy"}
	defer func() {
		for _, name := range names {
			_, _ = dockercli.Run(context.Background(), nil, "rm", "-f", name)
			_, _ = dockercli.Run(context.Background(), nil, "volume", "rm", name)
		}
	}()
	for i, name := range names {
		labels := []string{"--label", "tinystack." + service + "=missing"}
		if i < 2 {
			identity := owner
			if i == 1 {
				identity += "-other"
			}

			labels = append(labels, "--label", "tinystack."+service+".owner="+identity)
		}

		args := append([]string{"volume", "create"}, labels...)
		if _, err = dockercli.Run(ctx, nil, append(args, name)...); err != nil {
			t.Fatal(err)
		}

		command := []string{"create", "--name", name}
		if i == 1 {
			command = []string{"run", "-d", "--name", name}
		}

		command = append(command, labels...)
		if _, err = dockercli.Run(ctx, nil, append(command, "alpine:3.21", "sleep", "300")...); err != nil {
			t.Fatal(err)
		}
	}

	cfg := testConfig()
	configure(&cfg)
	server, stop := serverStore(t, cfg, &fakeRuntime{}, store)
	defer stop()
	for ctx.Err() == nil && (dockerExists(ctx, "container", names[0]) ||
		service != "elasticache" && dockerExists(ctx, "volume", names[0])) {
		time.Sleep(100 * time.Millisecond)
	}

	if ctx.Err() != nil {
		t.Fatal("owned orphan was not removed")
	}

	response, err := http.Post(server.URL+"/_tinystack/reset", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal(response.Status)
	}

	stop()
	for _, name := range names[1:] {
		if !dockerExists(ctx, "container", name) || !dockerExists(ctx, "volume", name) {
			t.Fatal("cleanup crossed store ownership:", name)
		}
	}

	if service == "elasticache" && !dockerExists(ctx, "volume", names[0]) {
		t.Fatal("ElastiCache removed an unused volume")
	}
}

func TestDockerRuntimeOwnersSurviveReset(t *testing.T) {
	ctx := context.Background()
	store := memory.New(nil, nil)
	owners := map[string]string{}
	for _, service := range []string{
		"rds", "opensearch", "elasticache", "mq", "batch", "codebuild", "memorydb", "bedrockagentcore", "dsql",
	} {
		key := "aws/" + service + "/runtime-owner"
		owner, err := dockercli.RuntimeOwner(ctx, store, key)
		if err != nil {
			t.Fatal(err)
		}

		owners[key] = owner
	}

	if err := store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, "ordinary-record", true)
	}); err != nil {
		t.Fatal(err)
	}

	server, stop := serverStore(t, testConfig(), &fakeRuntime{}, store)
	defer stop()
	response, err := http.Post(server.URL+"/_tinystack/reset", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal(response.Status)
	}

	for key, owner := range owners {
		if err = store.View(ctx, func(tx storage.ReadTx) error {
			recovered, found, readErr := storage.Read[string](tx, key)
			if readErr != nil || !found || *recovered != owner {
				t.Fatal("reset changed the runtime identity", key, recovered, readErr)
			}

			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err = store.View(ctx, func(tx storage.ReadTx) error {
		if len(tx.Keys("")) != len(owners) {
			t.Fatal("reset retained ordinary records")
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
