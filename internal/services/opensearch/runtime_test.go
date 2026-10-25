package opensearch_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/opensearch"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/opensearch"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

type removal struct {
	name   string
	volume bool
}

type fakeRuntime struct {
	failure error
	ensured []*opensearch.Spec
	removed []removal
	orphans []*opensearch.Spec
	mu      sync.Mutex
	ready   bool
	closed  bool
}

func (r *fakeRuntime) Resources(context.Context) ([]*opensearch.Spec, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.orphans, nil
}

func (r *fakeRuntime) Ensure(_ context.Context, spec *opensearch.Spec) (opensearch.Started, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != nil {
		return opensearch.Started{}, r.failure
	}

	r.ensured = append(r.ensured, spec)

	return opensearch.Started{ID: "container-" + spec.Name, Port: 19200}, nil
}

func (r *fakeRuntime) Probe(context.Context, int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ready {
		return errors.New("not ready")
	}

	return nil
}

func (r *fakeRuntime) Remove(_ context.Context, spec *opensearch.Spec, volume bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = append(r.removed, removal{spec.Name + spec.Volume, volume})

	return nil
}

func (r *fakeRuntime) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
}

func (r *fakeRuntime) become(ready bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ready = ready
}

func (r *fakeRuntime) snapshot() (ensured []*opensearch.Spec, removed []removal) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.ensured), slices.Clone(r.removed)
}

func dockerService(t *testing.T, store storage.Store, rt *fakeRuntime) *opensearch.Service {
	t.Helper()
	svc := opensearch.New(store)
	if err := svc.UseRuntime(rt, nil); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(svc.Stop)

	return svc
}

func describe(t *testing.T, svc *opensearch.Service, name string) (*api.DomainStatus, error) {
	t.Helper()
	out, err := svc.DescribeDomain(testContext(), &api.DescribeDomainRequest{DomainName: new(name)})
	if err != nil {
		return nil, err
	}

	return out.DomainStatus, nil
}

// eventually polls until check holds, for the poller's one-second ticks.
func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(50 * time.Millisecond)
	}
}

func awaitReady(t *testing.T, svc *opensearch.Service, name string) *api.DomainStatus {
	t.Helper()
	var status *api.DomainStatus
	eventually(t, name+" to stop processing", func() bool {
		var err error
		status, err = describe(t, svc, name)
		return err == nil && !common.Value(status.Processing)
	})

	return status
}

func TestDockerDomainLifecycle(t *testing.T) {
	ctx, rt := testContext(), &fakeRuntime{}
	svc := dockerService(t, memory.New(nil, nil), rt)

	created, err := svc.CreateDomain(ctx, &api.CreateDomainRequest{DomainName: new("logs")})
	if err != nil {
		t.Fatal(err)
	}

	if status := created.DomainStatus; !*status.Created || !*status.Processing || status.Endpoint != nil {
		t.Fatalf("a new domain processes without an endpoint: %+v", status)
	}

	if _, err = svc.CreateDomain(ctx, &api.CreateDomainRequest{DomainName: new("logs")}); err == nil {
		t.Fatal("expected a duplicate domain to be rejected")
	}

	// The engine does not answer yet: the domain keeps processing and publishes no endpoint.
	eventually(t, "the container to start", func() bool { ensured, _ := rt.snapshot(); return len(ensured) > 0 })
	if status, _ := describe(t, svc, "logs"); !*status.Processing || status.Endpoint != nil {
		t.Fatalf("an unhealthy engine must not publish an endpoint: %+v", status)
	}

	ensured, _ := rt.snapshot()
	spec := ensured[0]
	if spec.Image != "opensearchproject/opensearch:2.11.0" || spec.ARN != *created.DomainStatus.ARN {
		t.Fatalf("default engine: %+v", spec)
	}

	rt.become(true)
	status := awaitReady(t, svc, "logs")
	if *status.Endpoint != "127.0.0.1:19200" {
		t.Fatalf("endpoint %s", *status.Endpoint)
	}

	// Configuration changes are records: the container is not touched and the endpoint stays.
	updated, err := svc.UpdateDomainConfig(ctx, &api.UpdateDomainConfigRequest{
		DomainName:    new("logs"),
		ClusterConfig: &api.ClusterConfig{InstanceCount: new(int64(3))},
	})
	if err != nil {
		t.Fatal(err)
	}

	if *updated.DomainConfig.ClusterConfig.Options.InstanceCount != 3 {
		t.Fatalf("update was not recorded: %+v", updated.DomainConfig.ClusterConfig)
	}

	if status, _ = describe(t, svc, "logs"); *status.Endpoint != "127.0.0.1:19200" || *status.Processing {
		t.Fatalf("an update must keep the endpoint: %+v", status)
	}

	// An engine that stops answering is started again and the domain processes until it does.
	rt.become(false)
	eventually(t, "the domain to process again", func() bool {
		status, _ := describe(t, svc, "logs")
		return *status.Processing && status.Endpoint == nil
	})
	rt.become(true)
	awaitReady(t, svc, "logs")

	// Deleting keeps the name while the container and volume are removed.
	deleted, err := svc.DeleteDomain(ctx, &api.DeleteDomainRequest{DomainName: new("logs")})
	if err != nil {
		t.Fatal(err)
	}

	if !*deleted.DomainStatus.Deleted || !*deleted.DomainStatus.Processing {
		t.Fatalf("a deleted domain processes until it is gone: %+v", deleted.DomainStatus)
	}

	eventually(t, "the domain to be gone", func() bool {
		_, describeErr := describe(t, svc, "logs")
		return describeErr != nil
	})

	_, removed := rt.snapshot()
	if !slices.ContainsFunc(removed, func(r removal) bool { return r.volume }) {
		t.Fatalf("deleting a domain removes its volume: %+v", removed)
	}

	if _, err = svc.CreateDomain(ctx, &api.CreateDomainRequest{DomainName: new("logs")}); err != nil {
		t.Fatalf("the name is free once the domain is gone: %v", err)
	}
}

func TestDockerDomainDeletingRejectsUpdates(t *testing.T) {
	ctx, rt := testContext(), &fakeRuntime{}
	svc := dockerService(t, memory.New(nil, nil), rt)
	if _, err := svc.CreateDomain(ctx, &api.CreateDomainRequest{DomainName: new("logs")}); err != nil {
		t.Fatal(err)
	}

	rt.become(true)
	awaitReady(t, svc, "logs")
	if _, err := svc.DeleteDomain(ctx, &api.DeleteDomainRequest{DomainName: new("logs")}); err != nil {
		t.Fatal(err)
	}

	// The poller may already have removed the record; either way a late update must not succeed.
	_, err := svc.UpdateDomainConfig(ctx, &api.UpdateDomainConfigRequest{
		DomainName:    new("logs"),
		ClusterConfig: &api.ClusterConfig{InstanceCount: new(int64(2))},
	})
	if err == nil {
		t.Fatal("expected an update of a deleted domain to fail")
	}
}

func TestDockerDomainEngineVersions(t *testing.T) {
	ctx, rt := testContext(), &fakeRuntime{}
	svc := dockerService(t, memory.New(nil, nil), rt)
	for _, version := range []string{"Elasticsearch_7.9", "OpenSearch_9.0"} {
		_, err := svc.CreateDomain(
			ctx,
			&api.CreateDomainRequest{DomainName: new("unsupported"), EngineVersion: new(version)},
		)
		if err == nil {
			t.Fatalf("%s: expected an unsupported engine version", version)
		}
	}

	if _, err := svc.DescribeDomain(ctx, &api.DescribeDomainRequest{DomainName: new("unsupported")}); err == nil {
		t.Fatal("a rejected domain must not exist")
	}

	if _, err := svc.CreateDomain(ctx, &api.CreateDomainRequest{
		DomainName: new("legacy"), EngineVersion: new("Elasticsearch_7.10"),
	}); err != nil {
		t.Fatal(err)
	}

	eventually(t, "the container to start", func() bool { ensured, _ := rt.snapshot(); return len(ensured) > 0 })
	ensured, _ := rt.snapshot()
	if ensured[0].Image != "docker.elastic.co/elasticsearch/elasticsearch-oss:7.10.2" {
		t.Fatalf("Elasticsearch_7.10 image %s", ensured[0].Image)
	}
}

func TestDockerDomainStartFailure(t *testing.T) {
	ctx, rt := testContext(), &fakeRuntime{failure: errors.New("pull access denied")}
	svc := dockerService(t, memory.New(nil, nil), rt)
	if _, err := svc.CreateDomain(ctx, &api.CreateDomainRequest{DomainName: new("broken")}); err != nil {
		t.Fatal(err)
	}

	status := awaitReady(t, svc, "broken")
	if status.Endpoint != nil {
		t.Fatalf("a failed domain has no endpoint: %+v", status)
	}

	if _, removed := rt.snapshot(); len(removed) == 0 || !removed[0].volume {
		t.Fatalf("a domain that never ran releases its volume: %+v", removed)
	}

	resources, err := svc.Resources(ctx)
	if err != nil || len(resources) != 1 {
		t.Fatalf("inventory: %+v %v", resources, err)
	}

	details, _ := resources[0].Details.(map[string]any)
	if details["Failure"] == nil || details["ExecutionReady"] != false || details["RecordsOnly"] != false {
		t.Fatalf("inventory of a failed domain: %+v", details)
	}

	// A failed domain is deleted like any other.
	if _, err = svc.DeleteDomain(ctx, &api.DeleteDomainRequest{DomainName: new("broken")}); err != nil {
		t.Fatal(err)
	}

	eventually(t, "the failed domain to be gone", func() bool {
		_, err := describe(t, svc, "broken")
		return err != nil
	})
}

func TestDockerDomainRecoversFromRecords(t *testing.T) {
	ctx, store := testContext(), memory.New(nil, nil)
	records := opensearch.New(store)
	created, err := records.CreateDomain(ctx, &api.CreateDomainRequest{DomainName: new("kept")})
	if err != nil {
		t.Fatal(err)
	}

	if *created.DomainStatus.Processing || created.DomainStatus.Endpoint == nil {
		t.Fatalf("a records domain is ready at once: %+v", created.DomainStatus)
	}

	rt := &fakeRuntime{ready: true}
	svc := dockerService(t, store, rt)
	status, err := describe(t, svc, "kept")
	if err != nil || (!*status.Processing && status.Endpoint != nil && *status.Endpoint != "127.0.0.1:19200") {
		t.Fatalf("a recovered domain forgets the endpoint of the previous runtime: %+v %v", status, err)
	}

	if status = awaitReady(t, svc, "kept"); *status.Endpoint != "127.0.0.1:19200" {
		t.Fatalf("recovered endpoint %s", *status.Endpoint)
	}
}

func TestDockerDomainOrphans(t *testing.T) {
	ctx := testContext()
	arn := "arn:aws:es:us-east-1:123456789012:domain/kept"
	rt := &fakeRuntime{orphans: []*opensearch.Spec{
		{Name: "stray-container", ARN: "arn:aws:es:us-east-1:123456789012:domain/gone"},
		{Volume: "stray-volume", ARN: "arn:aws:es:us-east-1:123456789012:domain/gone"},
		{Name: "live-container", Volume: "live-volume", ARN: arn},
	}}
	svc := dockerService(t, memory.New(nil, nil), rt)
	if _, err := svc.CreateDomain(ctx, &api.CreateDomainRequest{DomainName: new("kept")}); err != nil {
		t.Fatal(err)
	}

	eventually(t, "the orphans to be removed", func() bool { _, removed := rt.snapshot(); return len(removed) >= 2 })
	_, removed := rt.snapshot()
	for _, name := range []string{"stray-container", "stray-volume"} {
		if !slices.ContainsFunc(removed, func(r removal) bool { return r.name == name && r.volume }) {
			t.Fatalf("%s was not removed with its volume: %+v", name, removed)
		}
	}

	if slices.ContainsFunc(removed, func(r removal) bool { return r.name == "live-container" }) {
		t.Fatalf("the container of an existing domain was removed: %+v", removed)
	}
}

func TestDockerDomainResetAndStop(t *testing.T) {
	ctx, rt := testContext(), &fakeRuntime{ready: true}
	svc := opensearch.New(memory.New(nil, nil))
	if err := svc.UseRuntime(rt, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.CreateDomain(ctx, &api.CreateDomainRequest{DomainName: new("one")}); err != nil {
		t.Fatal(err)
	}

	awaitReady(t, svc, "one")
	svc.Reset(ctx)
	_, removed := rt.snapshot()
	if !slices.ContainsFunc(removed, func(r removal) bool { return r.volume }) {
		t.Fatalf("a reset removes containers and volumes: %+v", removed)
	}

	svc.Stop()
	if !rt.closed {
		t.Fatal("stopping the service closes the runtime")
	}
}

func TestRecordsDomainIsUnchanged(t *testing.T) {
	svc := testService()
	created, err := svc.CreateDomain(testContext(), &api.CreateDomainRequest{DomainName: new("plain")})
	if err != nil {
		t.Fatal(err)
	}

	if *created.DomainStatus.Processing || created.DomainStatus.Endpoint == nil {
		t.Fatalf("records mode settles at once: %+v", created.DomainStatus)
	}

	deleted, err := svc.DeleteDomain(testContext(), &api.DeleteDomainRequest{DomainName: new("plain")})
	if err != nil || *deleted.DomainStatus.Processing || !*deleted.DomainStatus.Deleted {
		t.Fatalf("records mode deletes at once: %+v %v", deleted, err)
	}

	if _, err = describe(t, svc, "plain"); err == nil {
		t.Fatal("expected the domain to be gone")
	}
}
