package integration_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	sdk "github.com/aws/aws-sdk-go-v2/service/memorydb"
	"github.com/aws/aws-sdk-go-v2/service/memorydb/types"
	"github.com/redis/go-redis/v9"

	"github.com/amjadjibon/tinystack/internal/app"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/localtls"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func awaitMemoryDB(t *testing.T, ctx context.Context, c *sdk.Client, name string) types.Cluster {
	t.Helper()
	for ctx.Err() == nil {
		out, err := c.DescribeClusters(
			ctx,
			&sdk.DescribeClustersInput{ClusterName: new(name), ShowShardDetails: new(true)},
		)
		if err != nil {
			t.Fatal(err)
		}

		if aws.ToString(out.Clusters[0].Status) == "available" {
			return out.Clusters[0]
		}

		time.Sleep(time.Second)
	}

	t.Fatalf("cluster %s not available: %v", name, ctx.Err())
	return types.Cluster{}
}

func TestMemoryDBDockerLifecycle(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dir := t.TempDir()
	store, err := sqlite.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = store.Close() }()
	cfg := testConfig()
	cfg.Storage.Mode, cfg.Storage.Path = config.StoragePersistent, dir
	cfg.MemoryDB.Clusters = config.CachesDocker
	s, stop := memoryDBDockerServer(t, new(cfg), store)
	t.Cleanup(func() { stop() })
	c := memoryDBClient(s, "test", "us-east-1")
	otherRegion := memoryDBClient(s, "test", "eu-west-1")
	empty, scopeErr := otherRegion.DescribeClusters(ctx, &sdk.DescribeClustersInput{})
	if scopeErr != nil || len(empty.Clusters) != 0 {
		t.Fatal(empty, scopeErr)
	}

	owner := ""
	if err = store.View(ctx, func(tx storage.ReadTx) error {
		value, _, readErr := storage.Read[string](tx, "aws/memorydb/runtime-owner")
		if value != nil {
			owner = *value
		}

		return readErr
	}); err != nil {
		t.Fatal(err)
	}

	label := "tinystack.memorydb." + owner
	t.Cleanup(func() {
		stop()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		containers, _ := dockercli.ListLabeled(cleanupCtx, label)
		for id := range containers {
			_, _ = dockercli.Run(cleanupCtx, nil, "rm", "-f", id)
		}

		volumes, _ := dockercli.ListLabeledVolumes(cleanupCtx, label)
		for volume := range volumes {
			_, _ = dockercli.Run(cleanupCtx, nil, "volume", "rm", volume)
		}
	})

	ca, err := localtls.Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.PEM)
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	network := ec2Client(s, "test", "us-east-1")
	vpc, err := network.CreateVpc(ctx, &ec2sdk.CreateVpcInput{CidrBlock: new("10.0.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}

	subnet, err := network.CreateSubnet(
		ctx,
		&ec2sdk.CreateSubnetInput{VpcId: vpc.Vpc.VpcId, CidrBlock: new("10.0.1.0/24")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateSubnetGroup(ctx, &sdk.CreateSubnetGroupInput{
		SubnetGroupName: new("engine-net"),
		Description:     new("runtime"), SubnetIds: []string{aws.ToString(subnet.Subnet.SubnetId)},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"alice", "bob", "outside"} {
		_, err = c.CreateUser(ctx, &sdk.CreateUserInput{
			UserName:     new(name),
			AccessString: new("on ~* &* +@all"),
			AuthenticationMode: &types.AuthenticationMode{
				Type:      types.InputAuthenticationTypePassword,
				Passwords: []string{memoryDBPassword},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.CreateUser(ctx, &sdk.CreateUserInput{
		UserName: new("iam-user"), AccessString: new("on ~* +@all"),
		AuthenticationMode: &types.AuthenticationMode{Type: types.InputAuthenticationTypeIam},
	})
	wantMemoryDBError(t, err, "InvalidParameterValueException")
	_, err = c.CreateACL(ctx, &sdk.CreateACLInput{ACLName: new("engine-acl"), UserNames: []string{"alice"}})
	if err != nil {
		t.Fatal(err)
	}

	created, err := c.CreateCluster(ctx, &sdk.CreateClusterInput{
		ClusterName: new("engine"),
		NodeType:    new("db.r6g.large"),
		ACLName: new(
			"engine-acl",
		),
		SubnetGroupName:     new("engine-net"),
		NumShards:           new(int32(2)),
		NumReplicasPerShard: new(int32(1)),
	})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToString(created.Cluster.Status) != "creating" {
		t.Fatal(created.Cluster)
	}

	cluster := awaitMemoryDB(t, ctx, c, "engine")
	address := fmt.Sprintf("127.0.0.1:%d", cluster.ClusterEndpoint.Port)
	if aws.ToString(cluster.ClusterEndpoint.Address) != "127.0.0.1" ||
		cluster.Shards[0].Nodes[0].Endpoint.Port != cluster.ClusterEndpoint.Port {
		t.Fatal(cluster)
	}

	client := redis.NewClusterClient(
		&redis.ClusterOptions{
			Addrs:     []string{address},
			Username:  "alice",
			Password:  memoryDBPassword,
			TLSConfig: tlsConfig,
		},
	)
	defer func() { _ = client.Close() }()
	for _, key := range []string{"a", "b", "orders:{c}", "different-slot"} {
		if err = client.Set(ctx, key, "durable", 0).Err(); err != nil {
			t.Fatal(err)
		}

		if value, e := client.Get(ctx, key).Result(); e != nil || value != "durable" {
			t.Fatal(value, e)
		}
	}

	// Exercise CLUSTER SLOTS explicitly; the convenience method is deprecated in favor of CLUSTER SHARDS.
	slotsCmd := redis.NewClusterSlotsCmd(ctx, "cluster", "slots")
	if err = client.Process(ctx, slotsCmd); err != nil {
		t.Fatal(err)
	}

	slots, err := slotsCmd.Result()
	if err != nil || len(slots) != 1 || slots[0].Start != 0 || slots[0].End != 16383 ||
		slots[0].Nodes[0].Addr != address {
		t.Fatal(slots, err)
	}

	denied := redis.NewClient(
		&redis.Options{Addr: address, Username: "outside", Password: memoryDBPassword, TLSConfig: tlsConfig},
	)
	if err = denied.Ping(ctx).Err(); err == nil {
		t.Fatal("outside user authenticated")
	}

	_ = denied.Close()
	_, err = c.UpdateACL(
		ctx,
		&sdk.UpdateACLInput{
			ACLName:           new("engine-acl"),
			UserNamesToAdd:    []string{"bob"},
			UserNamesToRemove: []string{"alice"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	bob := redis.NewClient(
		&redis.Options{Addr: address, Username: "bob", Password: memoryDBPassword, TLSConfig: tlsConfig},
	)
	defer func() { _ = bob.Close() }()
	waitFor(t, "ACL update", func() bool { return bob.Ping(ctx).Err() == nil })
	alice := redis.NewClient(
		&redis.Options{Addr: address, Username: "alice", Password: memoryDBPassword, TLSConfig: tlsConfig},
	)
	if err = alice.Ping(ctx).Err(); err == nil {
		t.Fatal("removed user authenticated")
	}

	_ = alice.Close()
	_, err = c.UpdateUser(
		ctx,
		&sdk.UpdateUserInput{UserName: new("bob"), AccessString: new("on ~* +@read +ping +cluster")},
	)
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "read-only user", func() bool { return bob.Set(ctx, "denied", "x", 0).Err() != nil })
	if value, e := bob.Get(ctx, "a").Result(); e != nil || value != "durable" {
		t.Fatal(value, e)
	}

	rotatedPassword := memoryDBPassword + "-rotated"
	_, err = c.UpdateUser(ctx, &sdk.UpdateUserInput{UserName: new("bob"), AuthenticationMode: &types.AuthenticationMode{
		Type: types.InputAuthenticationTypePassword, Passwords: []string{rotatedPassword},
	}})
	if err != nil {
		t.Fatal(err)
	}

	rotated := redis.NewClient(
		&redis.Options{Addr: address, Username: "bob", Password: rotatedPassword, TLSConfig: tlsConfig},
	)
	waitFor(t, "password rotation", func() bool { return rotated.Ping(ctx).Err() == nil })
	_ = rotated.Close()
	oldPassword := redis.NewClient(
		&redis.Options{Addr: address, Username: "bob", Password: memoryDBPassword, TLSConfig: tlsConfig},
	)
	if err = oldPassword.Ping(ctx).Err(); err == nil {
		t.Fatal("old password authenticated after rotation")
	}

	_ = oldPassword.Close()
	_, err = c.UpdateCluster(ctx, &sdk.UpdateClusterInput{ClusterName: new("engine"), ACLName: new("open-access")})
	if err != nil {
		t.Fatal(err)
	}

	open := redis.NewClient(&redis.Options{Addr: address, TLSConfig: tlsConfig})
	waitFor(t, "cluster ACL switch", func() bool { return open.Ping(ctx).Err() == nil })
	_ = open.Close()
	_, err = c.UpdateCluster(ctx, &sdk.UpdateClusterInput{ClusterName: new("engine"), ACLName: new("engine-acl")})
	if err != nil {
		t.Fatal(err)
	}

	freshBob := redis.NewClient(
		&redis.Options{Addr: address, Username: "bob", Password: rotatedPassword, TLSConfig: tlsConfig},
	)
	waitFor(t, "cluster ACL restoration", func() bool { return freshBob.Ping(ctx).Err() == nil })
	_ = freshBob.Close()
	_, err = c.UpdateUser(ctx, &sdk.UpdateUserInput{UserName: new("bob"), AccessString: new("on +not-a-real-command")})
	wantMemoryDBError(t, err, "InvalidParameterValueException")
	containers, err := dockercli.ListLabeled(ctx, label)
	if err != nil || len(containers) != 1 {
		t.Fatal(containers, err)
	}

	volumes, err := dockercli.ListLabeledVolumes(ctx, label)
	if err != nil || len(volumes) != 1 {
		t.Fatal(volumes, err)
	}

	stop()
	if remaining, e := dockercli.ListLabeled(ctx, label); e != nil || len(remaining) != 0 {
		t.Fatal(remaining, e)
	}

	if remaining, e := dockercli.ListLabeledVolumes(ctx, label); e != nil || len(remaining) != 1 {
		t.Fatal(remaining, e)
	}

	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = sqlite.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	s, stop = memoryDBDockerServer(t, new(cfg), store)
	c = memoryDBClient(s, "test", "us-east-1")
	cluster = awaitMemoryDB(t, ctx, c, "engine")
	restarted := redis.NewClusterClient(
		&redis.ClusterOptions{
			Addrs:     []string{fmt.Sprintf("127.0.0.1:%d", cluster.ClusterEndpoint.Port)},
			Username:  "bob",
			Password:  rotatedPassword,
			TLSConfig: tlsConfig,
		},
	)
	defer func() { _ = restarted.Close() }()
	if value, e := restarted.Get(ctx, "a").Result(); e != nil || value != "durable" {
		t.Fatal(value, e)
	}

	_, err = c.DeleteCluster(ctx, &sdk.DeleteClusterInput{ClusterName: new("engine")})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "cluster deletion", func() bool {
		_, e := c.DescribeClusters(ctx, &sdk.DescribeClustersInput{ClusterName: new("engine")})
		return e != nil
	})
	if remaining, e := dockercli.ListLabeled(ctx, label); e != nil || len(remaining) != 0 {
		t.Fatal(remaining, e)
	}

	if remaining, e := dockercli.ListLabeledVolumes(ctx, label); e != nil || len(remaining) != 0 {
		t.Fatal(remaining, e)
	}

	_, err = c.CreateCluster(
		ctx,
		&sdk.CreateClusterInput{
			ClusterName: new("reset-me"),
			NodeType:    new("db.r6g.large"),
			ACLName:     new("open-access"),
			TLSEnabled:  new(false),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_ = awaitMemoryDB(t, ctx, c, "reset-me")
	if code, body := post(t, s, "/_tinystack/reset", nil); code != http.StatusOK {
		t.Fatal(code, body)
	}

	if remaining, e := dockercli.ListLabeledVolumes(ctx, label); e != nil || len(remaining) != 0 {
		t.Fatal(remaining, e)
	}

	if remaining, e := dockercli.ListLabeled(ctx, label); e != nil || len(remaining) != 0 {
		t.Fatal(remaining, e)
	}
}

func TestMemoryDBDockerRedisOpenAccess(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1")
	}

	cfg := testConfig()
	cfg.MemoryDB.Clusters = config.CachesDocker
	cfg.Storage.Mode, cfg.Storage.Path = config.StoragePersistent, t.TempDir()
	store := &memory.Store{}
	s, stop := memoryDBDockerServer(t, new(cfg), store)
	t.Cleanup(func() {
		stop()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var owner string
		_ = store.View(cleanupCtx, func(tx storage.ReadTx) error {
			value, _, err := storage.Read[string](tx, "aws/memorydb/runtime-owner")
			if value != nil {
				owner = *value
			}

			return err
		})
		if owner == "" {
			return
		}

		label := "tinystack.memorydb." + owner
		containers, _ := dockercli.ListLabeled(cleanupCtx, label)
		for id := range containers {
			_, _ = dockercli.Run(cleanupCtx, nil, "rm", "-f", id)
		}

		volumes, _ := dockercli.ListLabeledVolumes(cleanupCtx, label)
		for volume := range volumes {
			_, _ = dockercli.Run(cleanupCtx, nil, "volume", "rm", volume)
		}
	})
	c := memoryDBClient(s, "test", "us-east-1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	for _, version := range []string{"6.2", "7.0", "7.1"} {
		t.Run(version, func(t *testing.T) {
			name := "redis-" + strings.ReplaceAll(version, ".", "-")
			_, err := c.CreateCluster(ctx, &sdk.CreateClusterInput{
				ClusterName: new(name), NodeType: new("db.r6g.large"),
				Engine: new("redis"), EngineVersion: new(version), ACLName: new("open-access"), TLSEnabled: new(false),
			})
			if err != nil {
				t.Fatal(err)
			}

			cluster := awaitMemoryDB(t, ctx, c, name)
			client := redis.NewClusterClient(
				&redis.ClusterOptions{Addrs: []string{fmt.Sprintf("127.0.0.1:%d", cluster.ClusterEndpoint.Port)}},
			)
			defer func() { _ = client.Close() }()
			if err = client.Set(ctx, "key", "value", 0).Err(); err != nil {
				t.Fatal(err)
			}

			if value, e := client.Get(ctx, "key").Result(); e != nil || value != "value" {
				t.Fatal(value, e)
			}

			_, err = c.UpdateCluster(
				ctx,
				&sdk.UpdateClusterInput{ClusterName: new(name), Engine: new("valkey"), NodeType: new("db.r6g.xlarge")},
			)
			if err != nil {
				t.Fatal(err)
			}

			if value, e := client.Get(ctx, "key").Result(); e != nil || value != "value" {
				t.Fatal(value, e)
			}

			_, err = c.DeleteCluster(ctx, &sdk.DeleteClusterInput{ClusterName: new(name)})
			if err != nil {
				t.Fatal(err)
			}

			waitFor(t, "Redis delete", func() bool {
				_, e := c.DescribeClusters(ctx, &sdk.DescribeClustersInput{ClusterName: new(name)})
				return e != nil
			})
		})
	}
}

// memoryDBDockerServer keeps engine startup failures visible in the Docker test log.
func memoryDBDockerServer(
	t *testing.T,
	cfg *config.Config,
	store storage.Store,
) (server *httptest.Server, stop func()) {
	t.Helper()
	a, err := app.New(
		context.Background(),
		*cfg,
		store,
		&fakeRuntime{},
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	)
	if err != nil {
		t.Fatal(err)
	}

	server = httptest.NewServer(a.Gateway)
	return server, func() { a.Stop(); server.Close() }
}
