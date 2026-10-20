package cloudwatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudwatch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func TestMetricStreamClaiming(t *testing.T) {
	for _, mode := range []string{"memory", "sqlite"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			var store storage.Store = &memory.Store{}
			if mode == "sqlite" {
				db, err := sqlite.Open(ctx, t.TempDir())
				if err != nil {
					t.Fatal(err)
				}

				t.Cleanup(func() { _ = db.Close() })
				store = db
			}

			s := New(store, nil)
			s.Stop()
			now := time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)
			s.now = func() time.Time { return now }
			scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
			ctx = request.WithMetadata(ctx, request.Metadata{Scope: scope})
			_, err := s.PutMetricStream(ctx, &api.PutMetricStreamInput{
				Name: new("test"), OutputFormat: new("json"),
				FirehoseArn:    new("arn:aws:firehose:us-east-1:000000000000:deliverystream/test"),
				RoleArn:        new("arn:aws:iam::000000000000:role/test"),
				ExcludeFilters: []api.MetricStreamFilter{{Namespace: new("App"), MetricNames: []string{"Hidden"}}},
			})
			if err != nil {
				t.Fatal(err)
			}

			publish := func(name string, at time.Time, value float64) {
				t.Helper()
				_, putErr := s.PutMetricData(ctx, &api.PutMetricDataInput{
					Namespace: new("App"),
					MetricData: []api.MetricDatum{
						{MetricName: new(name), Timestamp: new(common.Unix(at)), Value: new(value)},
					},
				})
				if putErr != nil {
					t.Fatal(putErr)
				}
			}

			pending := func() []*streamPoint {
				t.Helper()
				var points []*streamPoint
				if viewErr := store.View(ctx, func(tx storage.ReadTx) error {
					var readErr error
					points, readErr = storage.Records[streamPoint](tx, streamPending(scope, "test"))
					return readErr
				}); viewErr != nil {
					t.Fatal(viewErr)
				}

				return points
			}

			publish("Visible", now, 2)
			publish("Visible", now.Add(10*time.Second), 6)
			publish("Hidden", now, 100)
			publish("TooOld", now.Add(-49*time.Hour), 100)
			points := pending()
			if len(points) != 1 || points[0].Point.Count != 2 || points[0].Point.Sum != 8 {
				t.Fatalf("pending: %+v", points)
			}

			// A denied claim is recorded once, with no replay after a later update to that minute.
			calls := 0
			s.authorizer = func(_ context.Context, d catalog.ServiceDelivery) error {
				calls++
				if d.Principal != "streams.metrics.cloudwatch.amazonaws.com" ||
					d.SourceARN != streamARN(scope, "test") ||
					d.SourceAccount != scope.AccountID ||
					d.Role.Operation != "PutRecordBatch" {
					t.Fatalf("delivery: %+v", d)
				}

				return errors.New("denied")
			}

			s.visitStreams(ctx)
			if calls != 0 {
				t.Fatal("uncompleted minute delivered")
			}

			now = now.Add(time.Minute)
			s.visitStreams(ctx)
			s.visitStreams(ctx)
			if calls != 1 || len(pending()) != 0 {
				t.Fatalf("claims: %d", calls)
			}

			publish("Visible", now.Add(-time.Minute), 10)
			if len(pending()) != 0 {
				t.Fatal("already sent timestamp requeued")
			}

			resources, err := s.Resources(ctx)
			if err != nil || len(resources) != 1 || resources[0].Details.(map[string]any)["Failed"] != int64(1) ||
				resources[0].Details.(map[string]any)["LastError"] != "denied" {
				t.Fatalf("resources: %+v, %v", resources, err)
			}

			_, err = s.StopMetricStreams(ctx, &api.StopMetricStreamsInput{Names: []string{"test"}})
			if err != nil {
				t.Fatal(err)
			}

			publish("Visible", now, 100)
			if len(pending()) != 0 {
				t.Fatal("stopped stream queued data")
			}

			_, err = s.StartMetricStreams(ctx, &api.StartMetricStreamsInput{Names: []string{"test"}})
			if err != nil {
				t.Fatal(err)
			}

			publish("Visible", now, 100)
			if len(pending()) != 1 {
				t.Fatal("started stream did not queue data")
			}

			_, err = s.DeleteMetricStream(ctx, &api.DeleteMetricStreamInput{Name: new("test")})
			if err != nil {
				t.Fatal(err)
			}

			if err := store.View(ctx, func(tx storage.ReadTx) error {
				if len(tx.Keys(streamIndex)) != 0 || len(tx.Keys(streamSent(scope, "test"))) != 0 ||
					len(tx.Keys(streamPending(scope, "test"))) != 0 {
					t.Fatal("deleted stream left queue/index state")
				}

				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
