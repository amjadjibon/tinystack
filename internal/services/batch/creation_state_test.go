package batch

import (
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/batch"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestCreateComputeEnvironmentRejectsDisabled(t *testing.T) {
	ctx := batchContext()
	s, managedInput := managedFixture(t)
	unmanagedInput := &api.CreateComputeEnvironmentRequest{
		ComputeEnvironmentName: new("unmanaged-disabled"),
		Type:                   new(unmanaged),
		ServiceRole:            new("batch-role"),
	}
	for _, in := range []*api.CreateComputeEnvironmentRequest{common.Clone(managedInput), unmanagedInput} {
		in.State = new(disabled)
		_, err := s.CreateComputeEnvironment(ctx, in)
		clientError(t, err)
	}
	rows, err := s.DescribeComputeEnvironments(ctx, &api.DescribeComputeEnvironmentsRequest{})
	if err != nil || len(rows.ComputeEnvironments) != 0 {
		t.Fatalf("rejected create left metadata: %v %v", rows, err)
	}
	// The default remains ENABLED and disabling is valid after creation.
	s = New(&memory.Store{}, nil)
	in := common.Clone(unmanagedInput)
	in.State = nil
	created, err := s.CreateComputeEnvironment(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.UpdateComputeEnvironment(ctx, &api.UpdateComputeEnvironmentRequest{
		ComputeEnvironment: created.ComputeEnvironmentArn,
		State:              new(disabled),
	})
	if err != nil {
		t.Fatal(err)
	}
}
