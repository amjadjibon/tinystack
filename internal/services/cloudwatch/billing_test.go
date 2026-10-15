package cloudwatch

import (
	"context"
	"sync"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudwatch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/costdata"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func TestBillingSampling(t *testing.T) {
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
			now := time.Date(2026, time.October, 31, 23, 59, 20, 0, time.UTC)
			s.now = func() time.Time { return now }
			scope := request.Scope{AccountID: "000000000000"}
			fixtureKey := scope.Key("costdata", "fixture", "test")
			fixture := costdata.Fixture{Input: costdata.Input{Lines: []costdata.Line{
				{Start: "2026-10-01", Service: "Amazon Simple Storage Service", UnblendedCost: "10.1"},
				{Start: "2026-10-02", Service: "Amazon Simple Storage Service", UnblendedCost: "-0.2"},
				{Start: "2026-10-03", Service: "unmapped", UnblendedCost: "0.1"},
				{Start: "2026-09-30", Service: "unmapped", UnblendedCost: "999"},
				{Start: "2026-11-01", Service: "unmapped", UnblendedCost: "888"},
			}}}
			write := func(dirty bool) {
				t.Helper()
				if err := store.Update(ctx, func(tx storage.WriteTx) error {
					if dirty {
						tx.Put(costdata.BillingIndexKey(scope), []byte("dirty"))
					}

					return storage.Write(tx, fixtureKey, fixture)
				}); err != nil {
					t.Fatal(err)
				}
			}
			points := func() []*point {
				t.Helper()
				var out []*point
				metricScope := request.Scope{AccountID: scope.AccountID, Region: "us-east-1"}
				id := metricID(billingNamespace, "EstimatedCharges", []api.Dimension{
					{Name: new("Currency"), Value: new("USD")},
				})
				if err := store.View(ctx, func(tx storage.ReadTx) error {
					var err error
					out, err = storage.Records[point](tx, dataPrefix(metricScope, id))
					return err
				}); err != nil {
					t.Fatal(err)
				}

				return out
			}
			sample := func() {
				t.Helper()
				if err := s.sampleBilling(ctx, scope.AccountID); err != nil {
					t.Fatal(err)
				}
			}

			// Upgrade from phase B: existing fixtures have no billing index yet.
			write(false)
			s.resumeBilling(ctx)
			s.visitBilling(ctx)
			got := points()
			if len(got) != 1 || got[0].Max != 10 || got[0].T != float64(now.Unix()) || got[0].Unit != unitNone {
				t.Fatalf("decimal total/current publish timestamp: %+v", got)
			}

			// Serializable claims deduplicate overlapping workers and service restarts.
			var workers sync.WaitGroup
			for range 10 {
				workers.Go(func() {
					if err := s.sampleBilling(ctx, scope.AccountID); err != nil {
						t.Error(err)
					}
				})
			}

			workers.Wait()
			restarted := New(store, nil)
			restarted.Stop()
			restarted.now = s.now
			if err := restarted.sampleBilling(ctx, scope.AccountID); err != nil {
				t.Fatal(err)
			}

			got = points()
			if len(got) != 1 {
				t.Fatalf("duplicate slot: %d", len(got))
			}

			// Dirty revisions publish promptly, even within an already sampled minute.
			fixture.Lines[0].UnblendedCost = "20.1"
			write(true)
			sample()
			sample()
			got = points()
			if len(got) != 2 || got[1].Max != 20 {
				t.Fatalf("fixture replacement: %+v", got)
			}

			// A UTC month change switches totals; a long Maximum window still sees October.
			now = time.Date(2026, time.November, 1, 0, 0, 1, 0, time.UTC)
			sample()
			got = points()
			if len(got) != 3 || got[2].Max != 888 {
				t.Fatalf("month rollover: %+v", got)
			}

			// Remove the future row: the new month's total becomes zero, keeping the fixture.
			fixture.Lines = fixture.Lines[:4]
			write(true)
			sample()
			metricScope := request.Scope{AccountID: scope.AccountID, Region: "us-east-1"}
			queryCtx := request.WithMetadata(ctx, request.Metadata{Scope: metricScope})
			out, err := s.GetMetricStatistics(queryCtx, &api.GetMetricStatisticsInput{
				Namespace:  new(billingNamespace),
				MetricName: new("EstimatedCharges"),
				Dimensions: []api.Dimension{{Name: new("Currency"), Value: new("USD")}},
				StartTime: new(
					float64(now.Add(-5 * time.Minute).Unix()),
				),
				EndTime:    new(float64(now.Add(time.Minute).Unix())),
				Period:     new(int64(300)),
				Statistics: []string{statMaximum},
			})
			if err != nil || len(out.Datapoints) != 2 ||
				*out.Datapoints[0].Maximum != 20 || *out.Datapoints[1].Maximum != 888 {
				t.Fatalf("Maximum retains earlier value in the same period: %+v %v", out, err)
			}

			// Empty fixtures keep publishing zero totals, but removed service series stop after one zero.
			fixture.Lines = nil
			write(true)
			sample()
			now = now.Add(time.Minute)
			sample()
			serviceID := metricID(billingNamespace, "EstimatedCharges", []api.Dimension{
				{Name: new("Currency"), Value: new("USD")},
				{Name: new("ServiceName"), Value: new("AmazonS3")},
			})
			if readErr := store.View(ctx, func(tx storage.ReadTx) error {
				servicePoints, recordsErr := storage.Records[point](tx, dataPrefix(metricScope, serviceID))
				if recordsErr != nil {
					return recordsErr
				}

				if len(servicePoints) != 5 || servicePoints[4].Max != 0 {
					t.Fatalf("removed service series: %+v", servicePoints)
				}

				return nil
			}); readErr != nil {
				t.Fatal(readErr)
			}

			// A later minute gets another zero; last-fixture deletion gets exactly one final zero.
			now = now.Add(time.Minute)
			sample()
			if err := store.Update(ctx, func(tx storage.WriteTx) error {
				tx.Delete(fixtureKey)
				tx.Put(costdata.BillingIndexKey(scope), []byte("dirty"))
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			sample()
			before := len(points())
			now = now.Add(time.Minute)
			sample()
			if got := points(); len(got) != before || got[len(got)-1].Max != 0 {
				t.Fatalf("last-fixture deletion: %+v", got)
			}

			// A worker that listed its target before reset must recheck the index before writing.
			if err := storage.Clear(ctx, store); err != nil {
				t.Fatal(err)
			}

			sample()
			if got := points(); len(got) != 0 {
				t.Fatalf("sample resurrected after reset: %+v", got)
			}
		})
	}
}
