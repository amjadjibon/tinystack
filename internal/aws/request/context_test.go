package request_test

import (
	"context"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/request"
)

func TestCallerARN(t *testing.T) {
	t.Parallel()

	ctx := request.WithMetadata(context.Background(), request.Metadata{
		Scope: request.Scope{AccountID: "123456789012", Region: "us-east-1"},
	})
	if got := request.CallerARN(ctx); got != "arn:aws:iam::123456789012:root" {
		t.Fatalf("expected root ARN, got %q", got)
	}

	principalCtx := request.WithMetadata(context.Background(), request.Metadata{
		Scope: request.Scope{AccountID: "123456789012", Region: "us-east-1"},
		Principal: &request.Principal{
			ARN: "arn:aws:iam::123456789012:role/admin",
		},
	})
	if got := request.CallerARN(principalCtx); got != "arn:aws:iam::123456789012:role/admin" {
		t.Fatalf("expected principal ARN, got %q", got)
	}
}
