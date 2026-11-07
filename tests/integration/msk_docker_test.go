package integration_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/service/kafka"
	"github.com/aws/aws-sdk-go-v2/service/kafka/types"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"

	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func mskDockerResources(ctx context.Context, t *testing.T, arn string) (containers, volumes []string) {
	t.Helper()
	cs, err := dockercli.ListLabeled(ctx, "tinystack.kafka")
	if err != nil {
		t.Fatal(err)
	}

	vs, err := dockercli.ListLabeledVolumes(ctx, "tinystack.kafka")
	if err != nil {
		t.Fatal(err)
	}

	for id, owner := range cs {
		if owner == arn {
			containers = append(containers, id)
		}
	}

	for id, owner := range vs {
		if owner == arn {
			volumes = append(volumes, id)
		}
	}

	return containers, volumes
}

// awaitMSKDockerGone waits for a cluster's containers and volumes to go: a broker start in flight when the cluster
// is deleted or reset finishes first, and the runtime visitor then removes what it started.
func awaitMSKDockerGone(ctx context.Context, t *testing.T, arn, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		cs, vs := mskDockerResources(ctx, t, arn)
		if len(cs) == 0 && len(vs) == 0 {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("%s leaked containers %v volumes %v", what, cs, vs)
		}

		time.Sleep(200 * time.Millisecond)
	}
}

func cleanupMSKDocker(ctx context.Context, t *testing.T, arn string) {
	t.Helper()
	ctx = context.WithoutCancel(ctx)
	cs, vs := mskDockerResources(ctx, t, arn)
	for _, id := range cs {
		_, _ = dockercli.Run(ctx, nil, "rm", "-f", id)
	}

	for _, id := range vs {
		_, _ = dockercli.Run(ctx, nil, "volume", "rm", id)
	}
}

func awaitMSKDocker(t *testing.T, ctx context.Context, c *sdk.Client, arn *string) *sdk.GetBootstrapBrokersOutput {
	t.Helper()
	for ctx.Err() == nil {
		out, err := c.DescribeCluster(ctx, &sdk.DescribeClusterInput{ClusterArn: arn})
		if err != nil {
			t.Fatal(err)
		}

		if out.ClusterInfo.State == types.ClusterStateFailed {
			ids, _ := mskDockerResources(ctx, t, *arn)
			for _, id := range ids {
				_ = dockercli.Stream(
					ctx,
					nil,
					func(line string) error { t.Log(line); return nil },
					"logs",
					"--tail",
					"40",
					id,
				)
			}

			t.Fatalf("cluster failed: %s", common.Value(out.ClusterInfo.StateInfo.Message))
		}

		if out.ClusterInfo.State == types.ClusterStateActive {
			brokers, err := c.GetBootstrapBrokers(ctx, &sdk.GetBootstrapBrokersInput{ClusterArn: arn})
			if err != nil {
				t.Fatal(err)
			}

			return brokers
		}

		time.Sleep(time.Second)
	}

	t.Fatal(ctx.Err())
	return nil
}

func kafkaMessage(t *testing.T, ctx context.Context, address string, tlsConfig *tls.Config, produce bool) {
	t.Helper()
	options := []kgo.Opt{
		kgo.SeedBrokers(address),
		kgo.ConsumeTopics("tinystack-test"),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	}

	if tlsConfig != nil {
		options = append(options, kgo.DialTLSConfig(tlsConfig))
	}

	client, err := kgo.NewClient(options...)
	if err != nil {
		t.Fatal(err)
	}

	defer client.Close()
	if produce {
		req := kmsg.NewPtrCreateTopicsRequest()
		topic := kmsg.NewCreateTopicsRequestTopic()
		topic.Topic, topic.NumPartitions, topic.ReplicationFactor = "tinystack-test", 1, 1
		req.Topics = append(req.Topics, topic)
		if _, err = client.Request(ctx, req); err != nil {
			t.Fatal(err)
		}

		if err = client.ProduceSync(ctx, &kgo.Record{Topic: "tinystack-test", Value: []byte("hello persistent kafka")}).
			FirstErr(); err != nil {
			t.Fatal(err)
		}
	}

	fetches := client.PollFetches(ctx)
	if errs := fetches.Errors(); len(errs) > 0 {
		t.Fatal(errs)
	}

	records := fetches.Records()
	if len(records) != 1 || string(records[0].Value) != "hello persistent kafka" {
		t.Fatalf("records %+v", records)
	}
}

func TestMSKDockerLifecycle(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1 for Redpanda")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	dir := t.TempDir()
	cfg := testConfig()
	cfg.Kafka.Clusters = config.ContainersDocker
	cfg.Storage.Mode, cfg.Storage.Path = config.StoragePersistent, dir
	store, err := sqlite.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	var s *httptest.Server
	var stop func()
	s, stop = serverStore(t, cfg, &fakeRuntime{}, store)
	t.Cleanup(func() { stop(); _ = store.Close() })
	c := mskClient(s, "test", "us-east-1")
	in := mskInput(t, s, "us-east-1")
	in.ClusterName = new(fmt.Sprintf("docker-%d", time.Now().UnixNano()))
	// Exercise both real listeners, with the advertised loopback ports selected before startup.
	in.EncryptionInfo = &types.EncryptionInfo{
		EncryptionInTransit: &types.EncryptionInTransit{ClientBroker: types.ClientBrokerTlsPlaintext},
	}

	created, err := c.CreateCluster(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	arn := *created.ClusterArn
	t.Cleanup(func() { stop(); cleanupMSKDocker(ctx, t, arn) })
	if created.State != types.ClusterStateCreating {
		t.Fatalf("state %s", created.State)
	}

	brokers := awaitMSKDocker(t, ctx, c, created.ClusterArn)
	nodes, err := c.ListNodes(ctx, &sdk.ListNodesInput{ClusterArn: created.ClusterArn})
	if err != nil || len(nodes.NodeInfoList) != 1 || len(nodes.NodeInfoList[0].BrokerNodeInfo.Endpoints) != 2 {
		t.Fatalf("%+v %v", nodes, err)
	}

	kafkaMessage(t, ctx, *brokers.BootstrapBrokerString, nil, true)
	// Trust TinyStack's actual CA rather than bypassing certificate validation.
	// #nosec G304 -- test-owned storage directory.
	ca, err := os.ReadFile(filepath.Join(dir, "tls", "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("invalid CA")
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	kafkaMessage(t, ctx, *brokers.BootstrapBrokerStringTls, tlsConfig, false)
	ctx = context.WithoutCancel(ctx)
	cs, vs := mskDockerResources(ctx, t, arn)
	if len(cs) != 1 || len(vs) != 1 {
		t.Fatalf("containers %v volumes %v", cs, vs)
	}

	stop()
	if dockerExists(ctx, "container", cs[0]) || !dockerExists(ctx, "volume", vs[0]) {
		t.Fatal("shutdown must remove container and keep volume")
	}

	// Recovery removes this store's orphans without touching another store's resources.
	owner, err := dockercli.Run(
		ctx,
		nil,
		"volume",
		"inspect",
		"--format",
		`{{index .Labels "tinystack.kafka.store"}}`,
		vs[0],
	)
	if err != nil {
		t.Fatal(err)
	}

	orphan, foreign := vs[0]+"-orphan", vs[0]+"-foreign"
	for _, volume := range []struct{ name, owner string }{{orphan, owner}, {foreign, "another-store"}} {
		if _, err = dockercli.Run(ctx, nil, "volume", "create", "--label", "tinystack.kafka=orphan",
			"--label", "tinystack.kafka.store="+volume.owner, volume.name); err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _, _ = dockercli.Run(context.WithoutCancel(ctx), nil, "volume", "rm", volume.name) })
	}

	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = sqlite.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	s, stop = serverStore(t, cfg, &fakeRuntime{}, store)
	c = mskClient(s, "test", "us-east-1")
	brokers = awaitMSKDocker(t, ctx, c, created.ClusterArn)
	if dockerExists(ctx, "volume", orphan) || !dockerExists(ctx, "volume", foreign) {
		t.Fatal("startup must remove owned orphans and retain other stores' volumes")
	}

	kafkaMessage(t, ctx, *brokers.BootstrapBrokerString, nil, false)
	kafkaMessage(t, ctx, *brokers.BootstrapBrokerStringTls, tlsConfig, false)
	if _, err = c.DeleteCluster(ctx, &sdk.DeleteClusterInput{ClusterArn: created.ClusterArn}); err != nil {
		t.Fatal(err)
	}

	awaitMSKDockerGone(ctx, t, arn, "delete")

	// Reset uses the same cleanup path and the runtime remains usable after reset.
	in = mskInput(t, s, "us-east-1")
	in.EncryptionInfo = &types.EncryptionInfo{
		EncryptionInTransit: &types.EncryptionInTransit{ClientBroker: types.ClientBrokerPlaintext},
	}

	created, err = c.CreateCluster(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	resetARN := *created.ClusterArn
	t.Cleanup(func() { stop(); cleanupMSKDocker(ctx, t, resetARN) })
	brokers = awaitMSKDocker(t, ctx, c, created.ClusterArn)
	kafkaMessage(t, ctx, *brokers.BootstrapBrokerString, nil, true)
	status, _ := post(t, s, "/_tinystack/reset", nil)
	if status != 200 {
		t.Fatal(status)
	}

	awaitMSKDockerGone(ctx, t, resetARN, "reset")

	if !dockerExists(ctx, "volume", foreign) {
		t.Fatal("reset removed another store's volume")
	}

	// Omitted encryption creates a TLS-only broker, including after a reset.
	in = mskInput(t, s, "us-east-1")
	created, err = c.CreateCluster(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	tlsARN := *created.ClusterArn
	t.Cleanup(func() { stop(); cleanupMSKDocker(ctx, t, tlsARN) })
	brokers = awaitMSKDocker(t, ctx, c, created.ClusterArn)
	if brokers.BootstrapBrokerString != nil || brokers.BootstrapBrokerStringTls == nil {
		t.Fatalf("%+v", brokers)
	}

	kafkaMessage(t, ctx, *brokers.BootstrapBrokerStringTls, tlsConfig, true)
	if _, err = c.DeleteCluster(ctx, &sdk.DeleteClusterInput{ClusterArn: created.ClusterArn}); err != nil {
		t.Fatal(err)
	}

	awaitMSKDockerGone(ctx, t, tlsARN, "TLS-only delete")
}

func TestMSKDockerRejectsUnsupportedAuthentication(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1")
	}

	cfg := testConfig()
	cfg.Kafka.Clusters = config.ContainersDocker
	s, stop := serverStore(t, cfg, &fakeRuntime{}, &memory.Store{})
	defer stop()
	c := mskClient(s, "test", "us-east-1")
	in := mskInput(t, s, "us-east-1")
	for _, auth := range []*types.ClientAuthentication{
		{Sasl: &types.Sasl{Iam: &types.Iam{Enabled: new(true)}}},
		{Sasl: &types.Sasl{Scram: &types.Scram{Enabled: new(true)}}},
		{Tls: &types.Tls{Enabled: new(true)}},
	} {
		in.ClientAuthentication = auth
		_, err := c.CreateCluster(context.Background(), in)
		mskStatus(t, err, "BadRequestException", 400)
	}
}
