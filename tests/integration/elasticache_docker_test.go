package integration_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	cachetypes "github.com/aws/aws-sdk-go-v2/service/elasticache/types"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	elasticacheapi "github.com/amjadjibon/tinystack/internal/aws/generated/elasticache"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/gateway"
	cache "github.com/amjadjibon/tinystack/internal/services/elasticache"
	cachedocker "github.com/amjadjibon/tinystack/internal/services/elasticache/docker"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

type fakeCacheNode struct {
	listener net.Listener
	spec     cache.NodeSpec
	port     int64
	running  bool
	removed  bool
}

type fakeCacheRuntime struct {
	nodes      map[string]*fakeCacheNode
	labeled    map[string]string
	configSets []string
	next       int
	mu         sync.Mutex
	failStarts bool
	closed     bool
}

func newFakeCacheRuntime() *fakeCacheRuntime {
	return &fakeCacheRuntime{
		nodes:   map[string]*fakeCacheNode{},
		labeled: map[string]string{},
	}
}

func (r *fakeCacheRuntime) Start(_ context.Context, spec *cache.NodeSpec) (cache.Started, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return cache.Started{}, errors.New("runtime is closed")
	}
	if r.failStarts {
		return cache.Started{}, errors.New("failed to start container")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return cache.Started{}, err
	}

	r.next++
	id := fmt.Sprintf("cache-cont-%d", r.next)
	port := int64(ln.Addr().(*net.TCPAddr).Port)

	node := &fakeCacheNode{
		spec:     *spec,
		listener: ln,
		port:     port,
		running:  true,
	}
	r.nodes[id] = node
	if arn := spec.Labels["tinystack.elasticache"]; arn != "" {
		r.labeled[id] = arn
	}

	go r.serveNode(ln)

	return cache.Started{ID: id, Port: port}, nil
}

func (r *fakeCacheRuntime) serveNode(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}

		go func(c net.Conn) {
			defer func() { _ = c.Close() }()
			reader := bufio.NewReader(c)
			for {
				line, rErr := reader.ReadString('\n')
				if rErr != nil {
					return
				}
				trimmed := strings.TrimSpace(line)
				switch {
				case strings.Contains(trimmed, "PING"):
					_, _ = c.Write([]byte("+PONG\r\n"))
				case strings.Contains(trimmed, "CONFIG"):
					// Read next line for SET
					setLine, _ := reader.ReadString('\n')
					r.mu.Lock()
					r.configSets = append(r.configSets, strings.TrimSpace(setLine))
					r.mu.Unlock()
					_, _ = c.Write([]byte("+OK\r\n"))
				case strings.HasPrefix(strings.ToLower(trimmed), "version"):
					_, _ = c.Write([]byte("VERSION 1.6.22\r\n"))
				}
			}
		}(conn)
	}
}

func (r *fakeCacheRuntime) Running(_ context.Context, id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	node, ok := r.nodes[id]
	if !ok || node.removed {
		return false, cache.ErrGone
	}

	return node.running, nil
}

func (r *fakeCacheRuntime) Remove(_ context.Context, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if node, ok := r.nodes[id]; ok {
		node.removed = true
		node.running = false
		if node.listener != nil {
			_ = node.listener.Close()
		}
	}
	delete(r.labeled, id)
}

func (r *fakeCacheRuntime) ListOwned(_ context.Context, label, ownerLabel, owner string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make(map[string]string, len(r.labeled))
	for k, v := range r.labeled {
		if node := r.nodes[k]; node != nil && node.spec.Labels[ownerLabel] == owner {
			out[k] = v
		}
	}
	return out, nil
}

func (r *fakeCacheRuntime) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closed = true
	for _, n := range r.nodes {
		if n.listener != nil {
			_ = n.listener.Close()
		}
		n.running = false
		n.removed = true
	}
	r.labeled = map[string]string{}
}

func (r *fakeCacheRuntime) stopNode(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if node, ok := r.nodes[id]; ok {
		node.running = false
		if node.listener != nil {
			_ = node.listener.Close()
		}
	}
}

func TestElastiCacheRuntime_ClusterLifecycle(t *testing.T) {
	store := &memory.Store{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	service := cache.New(store)
	rt := newFakeCacheRuntime()
	service.UseRuntime(rt, logger)
	h, err := gateway.New(config.Default(), logger, []catalog.Service{
		elasticacheapi.Descriptor(service),
	}...)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(h)
	t.Cleanup(func() {
		service.Stop()
		server.Close()
	})

	client := elasticache.New(elasticache.Options{
		BaseEndpoint: aws.String(server.URL),
		Region:       "us-east-1",
		Credentials:  aws.AnonymousCredentials{},
	})

	ctx := context.Background()

	// 1. Create custom parameter group
	pgName := "test-runtime-pg"
	_, err = client.CreateCacheParameterGroup(ctx, &elasticache.CreateCacheParameterGroupInput{
		CacheParameterGroupName:   aws.String(pgName),
		CacheParameterGroupFamily: aws.String("redis7"),
		Description:               aws.String("runtime test params"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2. Create Cache Cluster
	clusterID := "runtime-redis"
	createOut, err := client.CreateCacheCluster(ctx, &elasticache.CreateCacheClusterInput{
		CacheClusterId:          aws.String(clusterID),
		Engine:                  aws.String("redis"),
		EngineVersion:           aws.String("7.0"),
		CacheNodeType:           aws.String("cache.t4g.micro"),
		NumCacheNodes:           aws.Int32(1),
		CacheParameterGroupName: aws.String(pgName),
	})
	if err != nil {
		t.Fatal(err)
	}

	if *createOut.CacheCluster.CacheClusterStatus != "creating" {
		t.Fatalf("expected creating status, got %s", *createOut.CacheCluster.CacheClusterStatus)
	}

	// 3. Wait for cluster to become available
	waitFor(t, "cluster "+clusterID+" to become available", func() bool {
		out, descErr := client.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{
			CacheClusterId:    aws.String(clusterID),
			ShowCacheNodeInfo: aws.Bool(true),
		})
		if descErr != nil || len(out.CacheClusters) == 0 {
			return false
		}
		c := out.CacheClusters[0]
		return *c.CacheClusterStatus == "available" && len(c.CacheNodes) > 0 && *c.CacheNodes[0].Endpoint.Port != 0
	})

	// 4. Verify node info
	descOut, err := client.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{
		CacheClusterId:    aws.String(clusterID),
		ShowCacheNodeInfo: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	cluster := descOut.CacheClusters[0]
	if len(cluster.CacheNodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(cluster.CacheNodes))
	}
	node := cluster.CacheNodes[0]
	if *node.CacheNodeStatus != "available" {
		t.Fatalf("node status = %s", *node.CacheNodeStatus)
	}
	if *node.Endpoint.Port == 0 {
		t.Fatalf("expected allocated host port, got 0")
	}

	// 5. Modify parameter group: dynamic param triggers CONFIG SET, static param triggers pending-reboot
	_, err = client.ModifyCacheParameterGroup(ctx, &elasticache.ModifyCacheParameterGroupInput{
		CacheParameterGroupName: aws.String(pgName),
		ParameterNameValues: []cachetypes.ParameterNameValue{
			{
				ParameterName:  aws.String("maxmemory-policy"),
				ParameterValue: aws.String("allkeys-lru"),
			},
			{
				ParameterName:  aws.String("databases"),
				ParameterValue: aws.String("32"),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Cluster parameter apply status should become pending-reboot
	descOut, err = client.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{
		CacheClusterId: aws.String(clusterID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if *descOut.CacheClusters[0].CacheParameterGroup.ParameterApplyStatus != "pending-reboot" {
		t.Fatalf("expected pending-reboot, got %s",
			*descOut.CacheClusters[0].CacheParameterGroup.ParameterApplyStatus)
	}

	// 6. Reboot cache cluster restarts node and clears pending-reboot
	_, err = client.RebootCacheCluster(ctx, &elasticache.RebootCacheClusterInput{
		CacheClusterId:       aws.String(clusterID),
		CacheNodeIdsToReboot: []string{"0001"},
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "cluster "+clusterID+" to settle back to available after reboot", func() bool {
		out, descErr := client.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{
			CacheClusterId: aws.String(clusterID),
		})
		if descErr != nil || len(out.CacheClusters) == 0 {
			return false
		}
		c := out.CacheClusters[0]
		return *c.CacheClusterStatus == "available" &&
			*c.CacheParameterGroup.ParameterApplyStatus == "in-sync"
	})

	// 7. Test crash restart: stop the running container
	rt.mu.Lock()
	var contID string
	for id, n := range rt.nodes {
		if n.running {
			contID = id
			break
		}
	}
	rt.mu.Unlock()

	if contID == "" {
		t.Fatal("no running container found")
	}

	rt.stopNode(contID)

	// Runner should notice it is gone, transition cluster to modifying, and start a new container
	waitFor(t, "cluster "+clusterID+" to recover after container stop", func() bool {
		out, descErr := client.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{
			CacheClusterId:    aws.String(clusterID),
			ShowCacheNodeInfo: aws.Bool(true),
		})
		if descErr != nil || len(out.CacheClusters) == 0 {
			return false
		}
		c := out.CacheClusters[0]
		return *c.CacheClusterStatus == "available"
	})

	// 8. Delete cache cluster: container is removed and cluster record disappears
	_, err = client.DeleteCacheCluster(ctx, &elasticache.DeleteCacheClusterInput{
		CacheClusterId: aws.String(clusterID),
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "cluster "+clusterID+" to be completely deleted", func() bool {
		_, descErr := client.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{
			CacheClusterId: aws.String(clusterID),
		})
		return descErr != nil
	})
}

func TestElastiCacheDockerLive(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1 to run cache containers in live Docker")
	}

	store := &memory.Store{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	service := cache.New(store)
	rt := cachedocker.New()
	service.UseRuntime(rt, logger)
	h, err := gateway.New(config.Default(), logger, []catalog.Service{
		elasticacheapi.Descriptor(service),
	}...)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(h)
	t.Cleanup(func() {
		service.Stop()
		server.Close()
	})

	client := elasticache.New(elasticache.Options{
		BaseEndpoint: aws.String(server.URL),
		Region:       "us-east-1",
		Credentials:  aws.AnonymousCredentials{},
	})

	ctx := context.Background()
	clusterID := "docker-redis-test"

	_, err = client.CreateCacheCluster(ctx, &elasticache.CreateCacheClusterInput{
		CacheClusterId: aws.String(clusterID),
		Engine:         aws.String("redis"),
		EngineVersion:  aws.String("7.0"),
		CacheNodeType:  aws.String("cache.t4g.micro"),
		NumCacheNodes:  aws.Int32(1),
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "live docker cluster to become available", func() bool {
		out, descErr := client.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{
			CacheClusterId:    aws.String(clusterID),
			ShowCacheNodeInfo: aws.Bool(true),
		})
		if descErr != nil || len(out.CacheClusters) == 0 {
			return false
		}
		c := out.CacheClusters[0]
		return *c.CacheClusterStatus == "available" && len(c.CacheNodes) > 0 && *c.CacheNodes[0].Endpoint.Port != 0
	})

	descOut, err := client.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{
		CacheClusterId:    aws.String(clusterID),
		ShowCacheNodeInfo: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	port := *descOut.CacheClusters[0].CacheNodes[0].Endpoint.Port
	// Test actual Redis SET / GET
	conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.FormatInt(int64(port), 10))
	if err != nil {
		t.Fatalf("failed to connect to live redis: %v", err)
	}
	defer conn.Close()

	if _, err = conn.Write([]byte("*3\r\n$3\r\nSET\r\n$3\r\nfoo\r\n$3\r\nbar\r\n")); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	resp, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("unexpected SET response: %s", resp)
	}

	// Delete
	_, err = client.DeleteCacheCluster(ctx, &elasticache.DeleteCacheClusterInput{
		CacheClusterId: aws.String(clusterID),
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "live docker cluster to be deleted", func() bool {
		_, descErr := client.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{
			CacheClusterId: aws.String(clusterID),
		})
		return descErr != nil
	})
}

func TestElastiCacheRuntimeOwnedOrphans(t *testing.T) {
	store := memory.New(nil, nil)
	ctx := context.Background()
	if err := store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, "aws/elasticache/runtime-owner", "store")
	}); err != nil {
		t.Fatal(err)
	}

	rt := newFakeCacheRuntime()
	for _, owner := range []string{"store", "foreign"} {
		rt.nodes[owner] = &fakeCacheNode{spec: cache.NodeSpec{
			Labels: map[string]string{"tinystack.elasticache.owner": owner},
		}}
		rt.labeled[owner] = "missing"
	}

	service := cache.New(store)
	if err := service.UseRuntime(rt, slog.Default()); err != nil {
		t.Fatal(err)
	}

	defer service.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rt.mu.Lock()
		removed, foreign := rt.nodes["store"].removed, rt.nodes["foreign"].removed
		rt.mu.Unlock()
		if foreign {
			t.Fatal("removed another store's container")
		}

		if removed {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("owned orphan was not removed")
}
