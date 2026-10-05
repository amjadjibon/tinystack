package ec2

import (
	"context"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestTerminatedInstancesDisappear(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	s := New(memory.New(nil, nil))
	s.now = func() time.Time { return now }
	ctx := request.WithMetadata(context.Background(), request.Metadata{
		Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"},
	})
	image := PublicParameters("us-east-1")["/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"]
	launched, err := s.RunInstances(ctx, &api.RunInstancesRequest{
		ImageId: &image, MinCount: new(int64(1)), MaxCount: new(int64(1)),
	})
	if err != nil {
		t.Fatal(err)
	}

	id := *launched.Instances[0].InstanceId
	if _, err = s.TerminateInstances(ctx, &api.TerminateInstancesRequest{InstanceIds: []string{id}}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		after time.Duration
		want  int
	}{{59 * time.Minute, 1}, {time.Hour, 0}} {
		now = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Add(tc.after)
		out, describeErr := s.DescribeInstances(ctx, &api.DescribeInstancesRequest{})
		if describeErr != nil || len(out.Reservations) != tc.want {
			t.Fatalf("after %v: got %+v, %v", tc.after, out, describeErr)
		}
	}

	_, err = s.StopInstances(ctx, &api.StopInstancesRequest{InstanceIds: []string{id}})
	if err == nil {
		t.Fatal("stopped a terminated instance that is gone")
	}

	// The next launch removes its record.
	if _, err = s.RunInstances(ctx, &api.RunInstancesRequest{
		ImageId: &image, MinCount: new(int64(1)), MaxCount: new(int64(1)),
	}); err != nil {
		t.Fatal(err)
	}

	var found bool
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, found = tx.Get((request.Scope{AccountID: "000000000000", Region: "us-east-1"}).Key("ec2", kindInstance, id))
		return nil
	})
	if err != nil || found {
		t.Fatalf("terminated instance record kept: %v", err)
	}
}
