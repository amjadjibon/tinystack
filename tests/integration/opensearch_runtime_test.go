package integration_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/opensearch"
	"github.com/aws/aws-sdk-go-v2/service/opensearch/types"
	"github.com/aws/smithy-go"

	opensearchapi "github.com/amjadjibon/tinystack/internal/aws/generated/opensearch"
	"github.com/amjadjibon/tinystack/internal/gateway"
	"github.com/amjadjibon/tinystack/internal/services/opensearch"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

// fakeEngines stands in for Docker: each container is an HTTP server answering cluster health, and the volumes
// outlive the fake the way Docker volumes outlive TinyStack.
type fakeEngines struct {
	volumes map[string]bool
	servers map[string]*httptest.Server
	specs   map[string]*opensearch.Spec
	mu      sync.Mutex
}

func newFakeEngines() *fakeEngines {
	return &fakeEngines{
		volumes: map[string]bool{}, servers: map[string]*httptest.Server{}, specs: map[string]*opensearch.Spec{},
	}
}

func (f *fakeEngines) Resources(context.Context) ([]*opensearch.Spec, error) { return nil, nil }

func (f *fakeEngines) Ensure(_ context.Context, spec *opensearch.Spec) (opensearch.Started, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	server := f.servers[spec.Name]
	if server == nil {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"status":"yellow"}`)
		}))
		f.servers[spec.Name] = server
	}

	f.volumes[spec.Volume], f.specs[spec.Name] = true, spec
	address, err := url.Parse(server.URL)
	if err != nil {
		return opensearch.Started{}, err
	}

	port, err := strconv.ParseInt(address.Port(), 10, 64)

	return opensearch.Started{ID: spec.Name, Port: port}, err
}

func (f *fakeEngines) Probe(_ context.Context, port int64) error {
	res, err := http.Get(
		fmt.Sprintf("http://127.0.0.1:%d/_cluster/health", port),
	) // #nosec G107 -- loopback test server.
	if err != nil {
		return err
	}

	return res.Body.Close()
}

func (f *fakeEngines) Remove(_ context.Context, spec *opensearch.Spec, volume bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if server := f.servers[spec.Name]; server != nil {
		server.Close()
		delete(f.servers, spec.Name)
	}

	if volume {
		delete(f.volumes, spec.Volume)
	}

	return nil
}

func (f *fakeEngines) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, server := range f.servers {
		server.Close()
		delete(f.servers, name)
	}
}

func (f *fakeEngines) running() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.servers)
}

func (f *fakeEngines) hasVolume(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.volumes[name]
}

func (f *fakeEngines) spec(name string) *opensearch.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, spec := range f.specs {
		if spec.ARN == "arn:aws:es:us-east-1:000000000000:domain/"+name {
			return spec
		}
	}

	return nil
}

func openSearchRuntimeServer(
	t *testing.T,
	store storage.Store,
	engines *fakeEngines,
) (server *httptest.Server, stop func()) {
	t.Helper()
	service := opensearch.New(store)
	if err := service.UseRuntime(engines, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}

	handler, err := gateway.New(testConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)),
		opensearchapi.Descriptor(service))
	if err != nil {
		t.Fatal(err)
	}

	server = httptest.NewServer(handler)
	stop = func() {
		service.Stop()
		server.Close()
	}

	t.Cleanup(stop)

	return server, stop
}

func openSearchDomain(ctx context.Context, t *testing.T, c *sdk.Client, name string) *types.DomainStatus {
	t.Helper()
	out, err := c.DescribeDomain(ctx, &sdk.DescribeDomainInput{DomainName: new(name)})
	if err != nil {
		t.Fatal(err)
	}

	return out.DomainStatus
}

func awaitOpenSearchDomain(ctx context.Context, t *testing.T, c *sdk.Client, name string) *types.DomainStatus {
	t.Helper()
	var status *types.DomainStatus
	waitFor(t, "domain "+name+" to stop processing", func() bool {
		status = openSearchDomain(ctx, t, c, name)
		return !aws.ToBool(status.Processing)
	})

	return status
}

func wantAPIError(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != code {
		t.Fatalf("expected %s, got %v", code, err)
	}
}

func TestOpenSearchRuntimeLifecycle(t *testing.T) {
	ctx, engines := context.Background(), newFakeEngines()
	s, _ := openSearchRuntimeServer(t, &memory.Store{}, engines)
	c := opensearchClient(s, "test", "us-east-1")

	// Versions without an engine image are rejected, and the domain is not created.
	for _, version := range []string{"Elasticsearch_7.9", "OpenSearch_9.0"} {
		_, err := c.CreateDomain(ctx, &sdk.CreateDomainInput{DomainName: new("legacy"), EngineVersion: new(version)})
		wantAPIError(t, err, "DisabledOperationException")
	}

	if _, err := c.DescribeDomain(ctx, &sdk.DescribeDomainInput{DomainName: new("legacy")}); err == nil {
		t.Fatal("a rejected domain must not exist")
	}

	// Fine-grained access control needs authentication TinyStack's data plane cannot provide.
	_, err := c.CreateDomain(ctx, &sdk.CreateDomainInput{
		DomainName: new("secured"),
		AdvancedSecurityOptions: &types.AdvancedSecurityOptionsInput{
			Enabled: new(true), InternalUserDatabaseEnabled: new(true),
		},
	})
	wantAPIError(t, err, "NotImplemented")

	created, err := c.CreateDomain(ctx, &sdk.CreateDomainInput{
		DomainName:     new("logs"),
		EngineVersion:  new("OpenSearch_2.11"),
		AccessPolicies: new(`{"Version":"2012-10-17","Statement":[]}`),
		TagList:        []types.Tag{{Key: new("env"), Value: new("dev")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if status := created.DomainStatus; !aws.ToBool(status.Created) || !aws.ToBool(status.Processing) ||
		status.Endpoint != nil {
		t.Fatalf("a new domain processes without an endpoint: %+v", status)
	}

	_, err = c.CreateDomain(ctx, &sdk.CreateDomainInput{DomainName: new("logs")})
	wantAPIError(t, err, "ResourceAlreadyExistsException")

	status := awaitOpenSearchDomain(ctx, t, c, "logs")
	res, err := http.Get("http://" + aws.ToString(status.Endpoint) + "/_cluster/health") // #nosec G107 -- loopback.
	if err != nil {
		t.Fatalf("the endpoint must accept connections: %v", err)
	}

	_ = res.Body.Close()
	if spec := engines.spec("logs"); spec == nil || spec.Image != "opensearchproject/opensearch:2.11.0" {
		t.Fatalf("engine container: %+v", spec)
	}

	// Access policies and tags are configuration and records, unchanged by the engine.
	described, err := c.DescribeDomainConfig(ctx, &sdk.DescribeDomainConfigInput{DomainName: new("logs")})
	if err != nil || aws.ToString(described.DomainConfig.AccessPolicies.Options) == "" {
		t.Fatalf("access policies are kept as configuration: %+v %v", described, err)
	}

	tags, err := c.ListTags(ctx, &sdk.ListTagsInput{ARN: status.ARN})
	if err != nil || len(tags.TagList) != 1 {
		t.Fatalf("tags: %+v %v", tags, err)
	}

	// Resizing is a record: the single container keeps running at the same endpoint.
	if _, err = c.UpdateDomainConfig(ctx, &sdk.UpdateDomainConfigInput{
		DomainName: new("logs"), ClusterConfig: &types.ClusterConfig{InstanceCount: new(int32(3))},
	}); err != nil {
		t.Fatal(err)
	}

	if after := openSearchDomain(ctx, t, c, "logs"); !aws.ToBool(after.Processing) ||
		aws.ToString(
			after.Endpoint,
		) != aws.ToString(
			status.Endpoint,
		) || aws.ToInt32(after.ClusterConfig.InstanceCount) != 3 {
		t.Fatalf("an update keeps the endpoint: %+v", after)
	}

	if _, upgradeErr := c.UpgradeDomain(
		ctx,
		&sdk.UpgradeDomainInput{DomainName: new("logs"), TargetVersion: new("OpenSearch_2.13")},
	); upgradeErr == nil ||
		!strings.Contains(upgradeErr.Error(), "Docker mode") {
		t.Fatalf("Docker upgrades must be explicitly rejected: %v", upgradeErr)
	}
	if _, advancedErr := c.UpdateDomainConfig(
		ctx,
		&sdk.UpdateDomainConfigInput{
			DomainName:      new("logs"),
			AdvancedOptions: map[string]string{"override_main_response_version": "true"},
		},
	); advancedErr == nil ||
		!strings.Contains(advancedErr.Error(), "Docker mode") {
		t.Fatalf("Docker advanced options must be rejected: %v", advancedErr)
	}
	if _, advancedErr := c.CreateDomain(
		ctx,
		&sdk.CreateDomainInput{
			DomainName:      new("custom-options"),
			AdvancedOptions: map[string]string{"override_main_response_version": "true"},
		},
	); advancedErr == nil ||
		!strings.Contains(advancedErr.Error(), "Docker mode") {
		t.Fatalf("Docker create advanced options must be rejected: %v", advancedErr)
	}

	awaitOpenSearchDomain(ctx, t, c, "logs")

	if engines.running() != 1 {
		t.Fatalf("%d containers, want 1", engines.running())
	}

	// Domains are account and Region scoped: another account's "logs" is its own domain with its own container.
	other := opensearchClient(s, "team-b", "eu-west-1")
	if _, err = other.CreateDomain(ctx, &sdk.CreateDomainInput{DomainName: new("logs")}); err != nil {
		t.Fatal(err)
	}

	otherStatus := awaitOpenSearchDomain(ctx, t, other, "logs")
	if aws.ToString(otherStatus.ARN) == aws.ToString(status.ARN) || engines.running() != 2 {
		t.Fatalf("account isolation: %s, %d containers", aws.ToString(otherStatus.ARN), engines.running())
	}

	if _, err = other.DeleteDomain(ctx, &sdk.DeleteDomainInput{DomainName: new("logs")}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the other account's container to go", func() bool { return engines.running() == 1 })
	if openSearchDomain(ctx, t, c, "logs").Endpoint == nil {
		t.Fatal("deleting another account's domain must not touch this one")
	}

	// Deleting keeps the name while the container and volume go.
	deleted, err := c.DeleteDomain(ctx, &sdk.DeleteDomainInput{DomainName: new("logs")})
	if err != nil {
		t.Fatal(err)
	}

	if !aws.ToBool(deleted.DomainStatus.Deleted) || !aws.ToBool(deleted.DomainStatus.Processing) {
		t.Fatalf("a deleted domain processes until it is gone: %+v", deleted.DomainStatus)
	}

	waitFor(t, "the domain to be gone", func() bool {
		_, describeErr := c.DescribeDomain(ctx, &sdk.DescribeDomainInput{DomainName: new("logs")})
		return describeErr != nil
	})
	_, err = c.DescribeDomain(ctx, &sdk.DescribeDomainInput{DomainName: new("logs")})
	wantAPIError(t, err, "ResourceNotFoundException")
	if engines.running() != 0 || engines.hasVolume(engines.spec("logs").Volume) {
		t.Fatal("deleting a domain removes its container and volume")
	}
}

func TestOpenSearchRuntimeElasticsearch(t *testing.T) {
	ctx, engines := context.Background(), newFakeEngines()
	s, _ := openSearchRuntimeServer(t, &memory.Store{}, engines)
	c := opensearchClient(s, "test", "us-east-1")
	if _, err := c.CreateDomain(ctx, &sdk.CreateDomainInput{
		DomainName: new("legacy"), EngineVersion: new("Elasticsearch_7.10"),
	}); err != nil {
		t.Fatal(err)
	}

	awaitOpenSearchDomain(ctx, t, c, "legacy")
	if spec := engines.spec("legacy"); spec == nil ||
		spec.Image != "docker.elastic.co/elasticsearch/elasticsearch-oss:7.10.2" {
		t.Fatalf("Elasticsearch_7.10 container: %+v", spec)
	}

	names, err := c.ListDomainNames(ctx, &sdk.ListDomainNamesInput{EngineType: types.EngineTypeElasticsearch})
	if err != nil || len(names.DomainNames) != 1 {
		t.Fatalf("Elasticsearch domains: %+v %v", names, err)
	}
}

func TestOpenSearchRuntimePersistentRestart(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer store.Close()
	engines := newFakeEngines()
	s, stop := openSearchRuntimeServer(t, store, engines)
	c := opensearchClient(s, "test", "us-east-1")
	if _, err = c.CreateDomain(ctx, &sdk.CreateDomainInput{DomainName: new("durable")}); err != nil {
		t.Fatal(err)
	}

	first := awaitOpenSearchDomain(ctx, t, c, "durable")
	volume := engines.spec("durable").Volume
	stop()
	if engines.running() != 0 || !engines.hasVolume(volume) {
		t.Fatalf("shutdown removes containers (%d running) and keeps volumes", engines.running())
	}

	// The restarted server finds the domain in its records and starts a container on the same volume.
	s, _ = openSearchRuntimeServer(t, store, engines)
	c = opensearchClient(s, "test", "us-east-1")
	if status := openSearchDomain(ctx, t, c, "durable"); !aws.ToBool(status.Processing) || status.Endpoint != nil {
		t.Fatalf("a restarted domain forgets the previous endpoint until its engine answers: %+v", status)
	}

	second := awaitOpenSearchDomain(ctx, t, c, "durable")
	if engines.spec("durable").Volume != volume || engines.running() != 1 {
		t.Fatalf("the domain must reattach to volume %s", volume)
	}

	if aws.ToString(second.ARN) != aws.ToString(first.ARN) || aws.ToString(second.Endpoint) == "" {
		t.Fatalf("restarted domain: %+v", second)
	}
}
