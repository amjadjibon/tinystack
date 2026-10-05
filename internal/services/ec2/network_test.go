package ec2

import (
	"context"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

// A VPC stored before route tables existed gets its main route table on the
// next request.
func TestStoredVPCsGetMainRouteTables(t *testing.T) {
	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope})
	s := New(memory.New(nil, nil))
	old := &vpc{ID: "vpc-0123456789abcdef0", CIDR: "10.0.0.0/16", Tenancy: tenancyDefault}
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Put(key(scope, kindDefaults, "vpc"), []byte("{}"))
		return write(tx, key(scope, kindVPC, old.ID), old)
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := s.DescribeRouteTables(ctx, &api.DescribeRouteTablesRequest{})
	if err != nil || len(out.RouteTables) != 1 || *out.RouteTables[0].VpcId != old.ID ||
		!*out.RouteTables[0].Associations[0].Main || *out.RouteTables[0].Routes[0].DestinationCidrBlock != old.CIDR {
		t.Fatalf("got %+v, %v", out, err)
	}
}
