package cloudformation

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudformation"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestPendingResponseSurvivesRestart(t *testing.T) {
	store := &memory.Store{}
	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope, Endpoint: "http://localhost"})
	first := New(store, nil)
	_, err := first.CreateStack(ctx, &api.CreateStackInput{StackName: new("pending"), TemplateBody: new(`Resources:
  Provider:
    Type: Custom::Restart
    Properties: {ServiceToken: 'arn:aws:sns:us-east-1:000000000000:provider'}
Outputs:
  Value: {Value: !GetAtt Provider.Secret}
`)})
	if err != nil {
		t.Fatal(err)
	}

	ref := stackRef{scope: scope, name: "pending"}
	// Claim the step, then simulate a successful asynchronous dispatch before exit.
	if more, nextErr := first.next(ctx, ref); nextErr != nil || !more {
		t.Fatalf("claim: %v %v", more, nextErr)
	}

	if commitErr := first.commit(ctx, ref, func(_ storage.WriteTx, st *stack) error {
		st.Op.Pending.Sent = true
		return nil
	}); commitErr != nil {
		t.Fatal(commitErr)
	}

	second := New(store, nil)
	if more, nextErr := second.next(ctx, ref); nextErr != nil || more {
		t.Fatalf("pending visit: %v %v", more, nextErr)
	}

	st, err := second.current(ctx, scope, "pending")
	if err != nil {
		t.Fatal(err)
	}

	p := st.Op.Pending
	response := CustomResourceResponse{
		Status:             "SUCCESS",
		StackID:            st.ID,
		RequestID:          p.RequestID,
		LogicalResourceID:  "Provider",
		PhysicalResourceID: "provider-id",
		NoEcho:             true,
		Data:               map[string]any{"Secret": "hidden"},
	}

	body, _ := json.Marshal(response)
	// Identity mismatches and invalid statuses must not consume the capability.
	bad := response
	bad.RequestID = "wrong"
	invalid, _ := json.Marshal(bad)
	if responseErr := second.RespondCustomResource(ctx, p.ResponseID, invalid); responseErr == nil {
		t.Fatal("accepted mismatched response")
	}

	bad = response
	bad.Status = "UNKNOWN"
	invalid, _ = json.Marshal(bad)
	if responseErr := second.RespondCustomResource(ctx, p.ResponseID, invalid); responseErr == nil {
		t.Fatal("accepted invalid status")
	}

	for range 2 {
		if responseErr := second.RespondCustomResource(ctx, p.ResponseID, body); responseErr != nil {
			t.Fatal(responseErr)
		}
	}

	bad = response
	bad.PhysicalResourceID = "different"
	invalid, _ = json.Marshal(bad)
	if responseErr := second.RespondCustomResource(ctx, p.ResponseID, invalid); responseErr == nil {
		t.Fatal("accepted conflicting retry")
	}

	for range 4 {
		more, nextErr := second.next(ctx, ref)
		if nextErr != nil {
			t.Fatal(nextErr)
		}

		if !more {
			break
		}
	}

	st, err = second.current(ctx, scope, "pending")
	if err != nil {
		t.Fatal(err)
	}

	if st.Status != statusCreateComplete || st.Op != nil || st.Resources["Provider"].Physical != "provider-id" ||
		len(st.Outputs) != 1 || st.Outputs[0].Value != "****" {
		t.Fatalf("resumed stack: %+v", st)
	}

	if responseErr := second.RespondCustomResource(ctx, p.ResponseID, body); responseErr == nil {
		t.Fatal("accepted expired capability")
	}
}

func TestPendingTimeoutSurvivesRestart(t *testing.T) {
	store := &memory.Store{}
	s := New(store, nil)
	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope, Endpoint: "http://localhost"})
	_, err := s.CreateStack(ctx, &api.CreateStackInput{
		StackName: new("timeout"), DisableRollback: new(true),
		TemplateBody: new(`Resources:
  Provider:
    Type: Custom::Timeout
    Properties: {ServiceToken: 'arn:aws:sns:us-east-1:000000000000:provider'}
`),
	})
	if err != nil {
		t.Fatal(err)
	}

	ref := stackRef{scope: scope, name: "timeout"}
	if _, nextErr := s.next(ctx, ref); nextErr != nil {
		t.Fatal(nextErr)
	}

	if commitErr := s.commit(ctx, ref, func(_ storage.WriteTx, st *stack) error {
		st.Op.Pending.Sent = true
		st.Op.Pending.Deadline = time.Unix(0, 0)
		return nil
	}); commitErr != nil {
		t.Fatal(commitErr)
	}

	restarted := New(store, nil)
	if _, nextErr := restarted.next(ctx, ref); nextErr != nil {
		t.Fatal(nextErr)
	}

	st, err := restarted.current(ctx, scope, "timeout")
	if err != nil {
		t.Fatal(err)
	}

	if st.Status != statusCreateFailed || st.Op != nil {
		t.Fatalf("timed out stack: %+v", st)
	}
}
