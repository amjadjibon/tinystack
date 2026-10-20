package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/recent"
)

func TestDeliveryRecord(t *testing.T) {
	for _, mode := range []string{config.AuthorizationSoft, config.AuthorizationEnforce} {
		for _, allowed := range []bool{true, false} {
			t.Run(mode+"/"+map[bool]string{true: "allowed", false: "denied"}[allowed], func(t *testing.T) {
				s := &Server{requests: recent.New[Record](maxRecords, "requests")}
				ctx := request.WithMetadata(context.Background(), request.Metadata{RequestID: "origin"})
				s.recordDelivery(ctx, &AuthorizationRecord{Mode: mode, Allowed: allowed, Message: "decision"},
					&deliveryEntry{
						Path: "arn:aws:iam::123456789012:role/target", Service: "sts", Operation: "AssumeRole",
						Fields: map[string]string{"role": "target", "source_arn": "source"},
					}, accessDenied)
				rec := s.requests.List()[0]
				if rec.Method != "DELIVER" || rec.RequestID == "" || rec.RequestID == "origin" ||
					rec.Service != "sts" || rec.Operation != "AssumeRole" {
					t.Fatalf("unexpected delivery: %+v", rec)
				}

				var fields map[string]string
				if err := json.Unmarshal([]byte(rec.RequestBody), &fields); err != nil {
					t.Fatal(err)
				}

				if fields["caused_by"] != "origin" || fields["role"] != "target" || fields["source_arn"] != "source" {
					t.Fatalf("unexpected context: %v", fields)
				}

				status := http.StatusOK
				if !allowed && mode == config.AuthorizationEnforce {
					status = http.StatusForbidden
				}

				if rec.Status != status ||
					(!allowed && (rec.ErrorCode != accessDenied || rec.ErrorMessage != "decision")) ||
					(allowed && rec.ErrorCode != "") {
					t.Fatalf("unexpected decision: %+v", rec)
				}
			})
		}
	}
}
