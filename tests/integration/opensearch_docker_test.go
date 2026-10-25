package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/opensearch"
	"github.com/aws/aws-sdk-go-v2/service/opensearch/types"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

const openSearchDockerTimeout = 12 * time.Minute

func requireOpenSearchDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1 for live OpenSearch containers")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := dockercli.Run(ctx, nil, "info"); err != nil {
		t.Skipf("Docker daemon unavailable: %v", err)
	}
}

func openSearchDockerConfig() config.Config {
	cfg := testConfig()
	cfg.OpenSearch.Domains = config.DomainsDocker

	return cfg
}

// awaitDockerDomain polls until the domain stops processing, which includes the first image pull.
func awaitDockerDomain(ctx context.Context, t *testing.T, c *sdk.Client, name string) *types.DomainStatus {
	t.Helper()
	for ctx.Err() == nil {
		out, err := c.DescribeDomain(ctx, &sdk.DescribeDomainInput{DomainName: new(name)})
		if err != nil {
			t.Fatal(err)
		}

		if !aws.ToBool(out.DomainStatus.Processing) {
			if out.DomainStatus.Endpoint == nil {
				t.Fatalf("domain %s failed to start", name)
			}

			return out.DomainStatus
		}

		time.Sleep(time.Second)
	}

	t.Fatalf("timed out waiting for domain %s: %v", name, ctx.Err())

	return nil
}

func engineRequest(
	ctx context.Context,
	t *testing.T,
	method, endpoint, path, body string,
) (status int, response string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, "http://"+endpoint+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}

	return res.StatusCode, string(data)
}

// indexAndSearch writes one document over HTTP and finds it again.
func indexAndSearch(ctx context.Context, t *testing.T, endpoint, index, title string) {
	t.Helper()
	status, body := engineRequest(ctx, t, http.MethodPut, endpoint, "/"+index+"/_doc/1?refresh=true",
		fmt.Sprintf(`{"title":%q}`, title))
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("index document: HTTP %d %s", status, body)
	}

	searchDocument(ctx, t, endpoint, index, title)
}

func searchDocument(ctx context.Context, t *testing.T, endpoint, index, title string) {
	t.Helper()
	status, body := engineRequest(ctx, t, http.MethodPost, endpoint, "/"+index+"/_search",
		fmt.Sprintf(`{"query":{"match":{"title":%q}}}`, title))
	var result struct {
		Hits struct {
			Hits []struct {
				Source struct{ Title string } `json:"_source"`
			}
		}
	}

	if err := json.Unmarshal([]byte(body), &result); err != nil || status != http.StatusOK ||
		len(result.Hits.Hits) != 1 || result.Hits.Hits[0].Source.Title != title {
		t.Fatalf("search: HTTP %d %s (%v)", status, body, err)
	}
}

func dockerExists(ctx context.Context, kind, name string) bool {
	_, err := dockercli.Run(ctx, nil, kind, "inspect", name)

	return err == nil
}

func dockerResources(ctx context.Context, t *testing.T, arn string) (containers, volumes []string) {
	t.Helper()
	found, err := dockercli.ListLabeled(ctx, "tinystack.opensearch")
	if err != nil {
		t.Fatal(err)
	}

	for id, owner := range found {
		if owner == arn {
			containers = append(containers, id)
		}
	}

	stored, err := dockercli.ListLabeledVolumes(ctx, "tinystack.opensearch")
	if err != nil {
		t.Fatal(err)
	}

	for name, owner := range stored {
		if owner == arn {
			volumes = append(volumes, name)
		}
	}

	return containers, volumes
}

// removeDockerResources is the last cleanup of a test: it removes what the domain left if the test failed before
// TinyStack did.
func removeDockerResources(t *testing.T, arn string) {
	t.Helper()
	ctx := context.Background()
	containers, volumes := dockerResources(ctx, t, arn)
	for _, id := range containers {
		_, _ = dockercli.Run(ctx, nil, "rm", "-f", id)
	}

	for _, name := range volumes {
		_, _ = dockercli.Run(ctx, nil, "volume", "rm", name)
	}
}

func TestOpenSearchDockerLifecycle(t *testing.T) {
	requireOpenSearchDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), openSearchDockerTimeout)
	defer cancel()
	store, err := sqlite.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer store.Close()
	s, stop := serverStore(t, openSearchDockerConfig(), &fakeRuntime{}, store)
	t.Cleanup(func() { stop() })
	c := opensearchClient(s, "test", "us-east-1")
	name := fmt.Sprintf("docker-%d", time.Now().UnixNano()%1_000_000_000_000)

	created, err := c.CreateDomain(ctx, &sdk.CreateDomainInput{
		DomainName: new(name), EngineVersion: new("OpenSearch_2.11"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if !aws.ToBool(created.DomainStatus.Processing) || created.DomainStatus.Endpoint != nil {
		t.Fatalf("a new domain processes: %+v", created.DomainStatus)
	}

	arn := aws.ToString(created.DomainStatus.ARN)
	t.Cleanup(func() { removeDockerResources(t, arn) })
	status := awaitDockerDomain(ctx, t, c, name)
	if !strings.HasPrefix(aws.ToString(status.Endpoint), "127.0.0.1:") {
		t.Fatalf("endpoint %s", aws.ToString(status.Endpoint))
	}

	containers, volumes := dockerResources(ctx, t, arn)
	if len(containers) != 1 || len(volumes) != 1 {
		t.Fatalf("containers %v volumes %v", containers, volumes)
	}

	indexAndSearch(ctx, t, aws.ToString(status.Endpoint), "notes", "hello opensearch")

	// Resizing is a record: the container and its data stay.
	if _, err = c.UpdateDomainConfig(ctx, &sdk.UpdateDomainConfigInput{
		DomainName: new(name), ClusterConfig: &types.ClusterConfig{InstanceCount: new(int32(2))},
	}); err != nil {
		t.Fatal(err)
	}

	updated := awaitDockerDomain(ctx, t, c, name)
	if aws.ToString(updated.Endpoint) != aws.ToString(status.Endpoint) ||
		aws.ToInt32(updated.ClusterConfig.InstanceCount) != 2 {
		t.Fatalf("update: %+v", updated)
	}

	searchDocument(ctx, t, aws.ToString(updated.Endpoint), "notes", "hello opensearch")

	// A restart on the same sqlite store removes the container at shutdown and recreates it on the volume.
	stop()
	if dockerExists(ctx, "container", containers[0]) {
		t.Fatal("shutdown removes the container")
	}

	if _, kept := dockerResources(ctx, t, arn); len(kept) != 1 {
		t.Fatalf("shutdown keeps the volume: %v", kept)
	}

	s, stop = serverStore(t, openSearchDockerConfig(), &fakeRuntime{}, store)
	t.Cleanup(func() { stop() })
	c = opensearchClient(s, "test", "us-east-1")
	restarted := awaitDockerDomain(ctx, t, c, name)
	searchDocument(ctx, t, aws.ToString(restarted.Endpoint), "notes", "hello opensearch")

	// Deleting removes the container and the volume.
	if _, err = c.DeleteDomain(ctx, &sdk.DeleteDomainInput{DomainName: new(name)}); err != nil {
		t.Fatal(err)
	}

	for ctx.Err() == nil {
		if _, err = c.DescribeDomain(ctx, &sdk.DescribeDomainInput{DomainName: new(name)}); err != nil {
			break
		}

		time.Sleep(time.Second)
	}

	if containers, volumes = dockerResources(ctx, t, arn); len(containers) != 0 || len(volumes) != 0 {
		t.Fatalf("deleting a domain removes its container %v and volume %v", containers, volumes)
	}
}

func TestOpenSearchDockerElasticsearchAndReset(t *testing.T) {
	requireOpenSearchDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), openSearchDockerTimeout)
	defer cancel()
	s, stop := serverStore(t, openSearchDockerConfig(), &fakeRuntime{}, &memory.Store{})
	t.Cleanup(func() { stop() })
	c := opensearchClient(s, "test", "us-east-1")
	name := fmt.Sprintf("es-%d", time.Now().UnixNano()%1_000_000_000_000)

	// Elasticsearch versions other than the OSS 7.10 have no image.
	_, err := c.CreateDomain(
		ctx,
		&sdk.CreateDomainInput{DomainName: new(name), EngineVersion: new("Elasticsearch_6.8")},
	)
	wantAPIError(t, err, "DisabledOperationException")
	created, err := c.CreateDomain(ctx, &sdk.CreateDomainInput{
		DomainName: new(name), EngineVersion: new("Elasticsearch_7.10"),
	})
	if err != nil {
		t.Fatal(err)
	}

	arn := aws.ToString(created.DomainStatus.ARN)
	t.Cleanup(func() { removeDockerResources(t, arn) })
	status := awaitDockerDomain(ctx, t, c, name)
	indexAndSearch(ctx, t, aws.ToString(status.Endpoint), "notes", "hello elasticsearch")

	if containers, volumes := dockerResources(ctx, t, arn); len(containers) != 1 || len(volumes) != 1 {
		t.Fatalf("containers %v volumes %v", containers, volumes)
	}

	// A reset removes every domain's container and volume with the records.
	if code, body := post(t, s, "/_tinystack/reset", nil); code != http.StatusOK {
		t.Fatalf("reset: HTTP %d %s", code, body)
	}

	if containers, volumes := dockerResources(ctx, t, arn); len(containers) != 0 || len(volumes) != 0 {
		t.Fatalf("a reset removes the container %v and volume %v", containers, volumes)
	}
}

func TestOpenSearchDockerCloudFormation(t *testing.T) {
	requireOpenSearchDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), openSearchDockerTimeout)
	defer cancel()
	s, stop := serverStore(t, openSearchDockerConfig(), &fakeRuntime{}, &memory.Store{})
	t.Cleanup(func() { stop() })
	cf, c := cfnClient(s), opensearchClient(s, "test", "us-east-1")
	stack := fmt.Sprintf("os-stack-%d", time.Now().UnixNano()%1_000_000_000_000)
	t.Cleanup(func() { removeDockerResources(t, "arn:aws:es:us-east-1:000000000000:domain/cfn-test-domain") })
	if _, err := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: &stack, TemplateBody: new(openSearchTemplate(1, "Dev"))},
	); err != nil {
		t.Fatal(err)
	}

	var st cfntypes.Stack
	for ctx.Err() == nil {
		out, err := cf.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: &stack})
		if err != nil {
			t.Fatal(err)
		}

		if st = out.Stacks[0]; !strings.HasSuffix(string(st.StackStatus), "_IN_PROGRESS") {
			break
		}

		time.Sleep(time.Second)
	}

	if st.StackStatus != cfntypes.StackStatusCreateComplete {
		t.Fatalf("stack is %s (%s)", st.StackStatus, aws.ToString(st.StackStatusReason))
	}

	// Creation waits for the engine, so the output is the endpoint that answers.
	endpoint := outputsOf(&st)["DomainEndpoint"]
	described := openSearchDomain(ctx, t, c, "cfn-test-domain")
	if aws.ToBool(described.Processing) || endpoint == "" || endpoint != aws.ToString(described.Endpoint) {
		t.Fatalf("DomainEndpoint %q, domain %+v", endpoint, described)
	}

	indexAndSearch(ctx, t, endpoint, "notes", "hello stack")
}
