package mediaconvert_test

import (
	"context"
	"errors"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/mediaconvert"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/services/mediaconvert"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestGeneratedValidation(t *testing.T) {
	store := memory.New(nil, nil)
	svc := mediaconvert.New(store, iam.New(store))
	defer svc.Stop()
	ctx := request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "123456789012", Region: "us-east-1"}},
	)
	descriptor := api.Descriptor(svc)
	for _, tt := range []struct {
		input           any
		name, operation string
	}{
		{name: "required name", operation: "CreateQueue", input: &api.CreateQueueRequest{}},
		{name: "queue status enum", operation: "CreateQueue", input: &api.CreateQueueRequest{
			Name: new("valid"), Status: new("INVALID"),
		}},
		{name: "required role", operation: "CreateJob", input: &api.CreateJobRequest{Settings: &api.JobSettings{}}},
		{name: "list range", operation: "ListJobs", input: &api.ListJobsRequest{MaxResults: new(int64(21))}},
		{name: "job priority range", operation: "CreateJob", input: &api.CreateJobRequest{
			Role: new("role"), Settings: &api.JobSettings{}, Priority: new(int64(51)),
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := descriptor.Operations[tt.operation].Invoke(ctx, tt.input)
			var aws *awserror.Error
			if !errors.As(err, &aws) || aws.Code != "BadRequestException" {
				t.Fatalf("expected BadRequestException, got %v", err)
			}
		})
	}
}
