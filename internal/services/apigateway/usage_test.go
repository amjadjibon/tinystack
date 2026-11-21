package apigateway

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestUsageQuotaWindows(t *testing.T) {
	intake := time.Date(2026, time.May, 31, 23, 59, 59, 0, time.UTC) // Sunday and the last day of the month.
	rollover := intake.Add(time.Second)
	for _, period := range []string{"DAY", "WEEK", "MONTH"} {
		t.Run(period, func(t *testing.T) {
			scope := request.Scope{Region: "us-east-1", AccountID: "000000000000"}
			s := New(new(memory.Store), nil, nil, nil)
			p := &usagePlan{
				Plan: api.UsagePlan{Id: new("plan"), Quota: &api.QuotaSettings{
					Period: new(period), Limit: new(int64(2)), Offset: new(int64(1)),
				}},
				Joined: map[string]time.Time{"key": intake},
			}
			check := func(now time.Time, denied bool) {
				t.Helper()
				var response *execute.Response
				err := s.store.Update(t.Context(), func(tx storage.WriteTx) error {
					_, result, err := s.admitKey(tx, scope, p, "key", nil, now)
					response = result
					return err
				})
				if err != nil || (denied && (response == nil || response.Status != http.StatusTooManyRequests)) ||
					(!denied && response != nil) {
					t.Fatalf("at %s response = %+v, err = %v, want denied = %t", now, response, err, denied)
				}
			}
			check(intake, false)
			check(intake, true)
			check(rollover, false)
			check(rollover, false)
			check(rollover, true)

			err := s.store.View(t.Context(), func(tx storage.ReadTx) error {
				start, end := intake.Truncate(24*time.Hour), rollover
				got, err := usageLog(tx, scope, p, "key", start, end)
				if err == nil && (len(got) != 2 || !slices.Equal(got[0], []int64{1, 0}) ||
					!slices.Equal(got[1], []int64{2, 0})) {
					t.Errorf("daily usage = %v, want [[1 0] [2 0]]", got)
				}

				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
