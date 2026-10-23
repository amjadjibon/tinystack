package globalaccelerator_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/globalaccelerator"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/globalaccelerator"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestModelValidation(t *testing.T) {
	svc := globalaccelerator.New(memory.New(nil, nil), nil, nil, nil)
	descriptor := api.Descriptor(svc)
	ctx := request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "123456789012", Region: "us-west-2"}},
	)
	for _, test := range []struct {
		input    any
		name, op string
	}{
		{name: "required name", op: "CreateAccelerator", input: &api.CreateAcceleratorRequest{
			IdempotencyToken: new("token"),
		}},
		{name: "required token", op: "CreateAccelerator", input: &api.CreateAcceleratorRequest{
			Name: new("name"),
		}},
		{name: "name length", op: "CreateAccelerator", input: &api.CreateAcceleratorRequest{
			Name:             new(strings.Repeat("x", 256)),
			IdempotencyToken: new("token"),
		}},
		{name: "ip enum", op: "CreateAccelerator", input: &api.CreateAcceleratorRequest{
			Name:             new("name"),
			IdempotencyToken: new("token"), IpAddressType: new("IPV6"),
		}},
		{name: "page range", op: "ListAccelerators", input: &api.ListAcceleratorsRequest{
			MaxResults: new(int64(0)),
		}},
		{name: "protocol enum", op: "CreateListener", input: &api.CreateListenerRequest{
			AcceleratorArn:   new("arn"),
			IdempotencyToken: new("token"),
			Protocol:         new("HTTP"),
			PortRanges:       []api.PortRange{},
		}},
		{name: "port range", op: "CreateListener", input: &api.CreateListenerRequest{
			AcceleratorArn:   new("arn"),
			IdempotencyToken: new("token"),
			Protocol:         new("TCP"),
			PortRanges:       []api.PortRange{{FromPort: new(int64(0)), ToPort: new(int64(1))}},
		}},
		{name: "weight range", op: "AddEndpoints", input: &api.AddEndpointsRequest{
			EndpointGroupArn:       new("arn"),
			EndpointConfigurations: []api.EndpointConfiguration{{EndpointId: new("i-id"), Weight: new(int64(256))}},
		}},
		{name: "health path pattern", op: "UpdateEndpointGroup", input: &api.UpdateEndpointGroupRequest{
			EndpointGroupArn: new("arn"),
			HealthCheckPath:  new("missing-slash"),
		}},
		{name: "traffic range", op: "UpdateEndpointGroup", input: &api.UpdateEndpointGroupRequest{
			EndpointGroupArn:      new("arn"),
			TrafficDialPercentage: new(float64(101)),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := descriptor.Operations[test.op].Invoke(ctx, test.input)
			var awsErr *awserror.Error
			if !errors.As(err, &awsErr) || awsErr.Code != "InvalidArgumentException" {
				t.Fatalf("want generated InvalidArgumentException, got %v", err)
			}
		})
	}
}
