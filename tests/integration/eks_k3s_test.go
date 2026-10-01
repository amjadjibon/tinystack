package integration_test

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	eksapi "github.com/amjadjibon/tinystack/internal/aws/generated/eks"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/gateway"
	compute "github.com/amjadjibon/tinystack/internal/services/ec2"
	kubernetes "github.com/amjadjibon/tinystack/internal/services/eks"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

// clusterRuntime runs API servers in memory: a cluster named "broken" fails
// to start.
type clusterRuntime struct {
	running map[string]bool
	started []kubernetes.Cluster
	mu      sync.Mutex
}

func (r *clusterRuntime) Start(_ context.Context, cluster kubernetes.Cluster) (kubernetes.Server, error) {
	if cluster.Name == "broken" {
		return kubernetes.Server{}, errors.New("k3s did not become ready")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, cluster)
	id := cluster.Name + "-server"
	r.running[id] = true
	return kubernetes.Server{ID: id, URL: "https://127.0.0.1:6443", CA: "-----BEGIN CERTIFICATE-----"}, nil
}

func (r *clusterRuntime) Running(_ context.Context, id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running[id], nil
}

func (r *clusterRuntime) Remove(_ context.Context, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.running, id)
}

func (r *clusterRuntime) Close() {}

func (r *clusterRuntime) isRunning(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running[id]
}

// eksServer serves EKS, running API servers in rt, and EC2.
func eksServer(t *testing.T, rt kubernetes.Runtime) *httptest.Server {
	t.Helper()
	store := &memory.Store{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	network := compute.New(store)
	clusters := kubernetes.New(store, network)
	clusters.UseRuntime(rt, logger)
	h, err := gateway.New(config.Default(), logger, []catalog.Service{
		ec2api.Descriptor(network), eksapi.Descriptor(clusters),
	}...)
	if err != nil {
		t.Fatal(err)
	}

	s := httptest.NewServer(h)
	t.Cleanup(func() {
		clusters.Stop()
		s.Close()
	})

	return s
}

func describeEKS(ctx context.Context, t *testing.T, c *eks.Client, name string) *ekstypes.Cluster {
	t.Helper()
	out, err := c.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: &name})
	if err != nil {
		t.Fatal(err)
	}

	return out.Cluster
}

func TestEKSClustersRunAPIServers(t *testing.T) {
	rt := &clusterRuntime{running: map[string]bool{}}
	s := eksServer(t, rt)
	ctx := context.Background()
	c := eksClient(s, "us-east-1")
	_, subnets := defaultSubnets(ctx, t, s)
	created := createEKSCluster(ctx, t, c, subnets, "demo", ekstypes.AuthenticationModeApi)
	if created.Status != ekstypes.ClusterStatusCreating {
		t.Fatalf("created = %+v", created)
	}

	var cl *ekstypes.Cluster
	waitFor(t, "an active cluster", func() bool {
		cl = describeEKS(ctx, t, c, "demo")
		return cl.Status == ekstypes.ClusterStatusActive
	})

	ca, _ := base64.StdEncoding.DecodeString(aws.ToString(cl.CertificateAuthority.Data))
	if aws.ToString(cl.Endpoint) != "https://127.0.0.1:6443" || string(ca) != "-----BEGIN CERTIFICATE-----" ||
		rt.started[0] != (kubernetes.Cluster{Account: "000000000000", Name: "demo", Version: "1.34"}) {
		t.Fatalf("cluster = %+v, started = %+v", cl, rt.started)
	}

	_, err := c.UpdateClusterVersion(ctx, &eks.UpdateClusterVersionInput{
		Name: aws.String("demo"), Version: aws.String("1.35"),
	})
	wantCode(t, err, "InvalidRequestException")

	// A server that stops, as every one does when TinyStack restarts, is
	// started again.
	rt.Remove(ctx, "demo-server")
	waitFor(t, "a restarted server", func() bool { return rt.isRunning("demo-server") })

	createEKSCluster(ctx, t, c, subnets, "broken", ekstypes.AuthenticationModeApi)
	waitFor(t, "a failed cluster", func() bool {
		return describeEKS(ctx, t, c, "broken").Status == ekstypes.ClusterStatusFailed
	})

	if _, err = c.DeleteCluster(ctx, &eks.DeleteClusterInput{Name: aws.String("demo")}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "a removed server", func() bool { return !rt.isRunning("demo-server") })
}

// eksToken makes the token "aws eks get-token" makes for a cluster.
func eksToken(t *testing.T, key, secret, cluster string, signedAt time.Time) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://sts.us-east-1.amazonaws.com/?Action=GetCallerIdentity&Version=2011-06-15&X-Amz-Expires=60",
		http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("X-K8s-Aws-Id", cluster)
	presigned, _, err := v4.NewSigner().PresignHTTP(context.Background(),
		aws.Credentials{AccessKeyID: key, SecretAccessKey: secret}, req,
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "sts", "us-east-1", signedAt)
	if err != nil {
		t.Fatal(err)
	}

	return "k8s-aws-v1." + base64.RawURLEncoding.EncodeToString([]byte(presigned))
}

func TestKubernetesAccount(t *testing.T) {
	cfg := config.Default()
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.Accounts = map[string]string{"team-b": "111111111111"}
	cfg.Identity.SecretKeys = map[string]string{"test": "secret", "team-b": "secret-b"}
	h, err := gateway.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	for _, tc := range []struct {
		name, token, account string
	}{
		{"default account", eksToken(t, "test", "secret", "demo", now), "000000000000"},
		{"mapped account", eksToken(t, "team-b", "secret-b", "demo", now), "111111111111"},
		// EKS accepts a token for 15 minutes, whatever X-Amz-Expires says.
		{"ten minutes old", eksToken(t, "test", "secret", "demo", now.Add(-10*time.Minute)), "000000000000"},
		{"too old", eksToken(t, "test", "secret", "demo", now.Add(-20*time.Minute)), ""},
		{"other cluster", eksToken(t, "test", "secret", "other", now), ""},
		{"wrong secret", eksToken(t, "test", "guess", "demo", now), ""},
		{"unknown key", eksToken(t, "nobody", "secret", "demo", now), ""},
		{"not a token", "k8s-aws-v1.bm90IGEgdXJs", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "https://127.0.0.1:6443/api", http.NoBody)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			account, accountErr := h.KubernetesAccount(req, "demo")
			if tc.account == "" {
				if !errors.Is(accountErr, gateway.ErrKubernetesToken) {
					t.Fatalf("account = %q, %v", account, accountErr)
				}

				return
			}

			if accountErr != nil || account != tc.account {
				t.Fatalf("account = %q, %v", account, accountErr)
			}
		})
	}
}
