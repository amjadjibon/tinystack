package inspector2_test

import (
	"context"
	"errors"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/inspector2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/inspector2"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestGeneratedValidation(t *testing.T) {
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: request.Scope{
		AccountID: "123456789012", Region: "us-east-1",
	}})
	s := inspector2.New(memory.New(nil, nil))
	descriptor := api.Descriptor(s)
	tests := []struct {
		input    any
		name, op string
	}{
		{name: "required", op: "CreateFilter", input: &api.CreateFilterRequest{}},
		{name: "invalid scan enum", op: "Enable", input: &api.EnableRequest{ResourceTypes: []string{"INVALID"}}},
		{name: "invalid account pattern", op: "Disable", input: &api.DisableRequest{AccountIds: []string{"bad"}}},
		{name: "configuration enum", op: "UpdateConfiguration", input: &api.UpdateConfigurationRequest{
			EcrConfiguration: &api.EcrConfiguration{RescanDuration: new("INVALID")},
		}},
		{name: "nested required", op: "CreateFilter", input: &api.CreateFilterRequest{
			Name: new("filter"), Action: new("NONE"),
			FilterCriteria: &api.FilterCriteria{Severity: []api.StringFilter{{Value: new("LOW")}}},
		}},
		{name: "max results", op: "ListFilters", input: &api.ListFiltersRequest{MaxResults: new(int64(0))}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := descriptor.Operations[tt.op].Invoke(ctx, tt.input)
			var apiErr *awserror.Error
			if !errors.As(err, &apiErr) || apiErr.Code != "ValidationException" {
				t.Fatalf("validation: %v", err)
			}
		})
	}
}

func TestEnableTokenReplay(t *testing.T) {
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: request.Scope{
		AccountID: "123456789012", Region: "us-east-1",
	}})
	s := inspector2.New(memory.New(nil, nil))
	enable := &api.EnableRequest{ResourceTypes: []string{"EC2"}, ClientToken: new("replay-token")}
	if _, err := s.Enable(ctx, enable); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Disable(ctx, &api.DisableRequest{}); err != nil {
		t.Fatal(err)
	}

	replay, err := s.Enable(ctx, enable)
	if err != nil || *replay.Accounts[0].Status != "ENABLED" {
		t.Fatalf("replay: %+v %v", replay, err)
	}

	status, err := s.BatchGetAccountStatus(ctx, &api.BatchGetAccountStatusRequest{})
	if err != nil || *status.Accounts[0].State.Status != "DISABLED" {
		t.Fatalf("replay changed state: %+v %v", status, err)
	}

	_, err = s.Enable(ctx, &api.EnableRequest{ResourceTypes: []string{"ECR"}, ClientToken: enable.ClientToken})
	var e *awserror.Error
	if !errors.As(err, &e) || e.Code != "ValidationException" {
		t.Fatalf("token mismatch: %v", err)
	}
}
