package ec2

import (
	"context"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

// A VPC stored before route tables and network ACLs existed gets its main
// route table and default ACL, with its subnets, on the next request.
func TestStoredVPCsGetRoutingDefaults(t *testing.T) {
	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope})
	s := New(memory.New(nil, nil))
	old := &vpc{ID: "vpc-0123456789abcdef0", CIDR: "10.0.0.0/16", Tenancy: tenancyDefault}
	sn := &subnet{ID: "subnet-0123456789abcdef0", VPCID: old.ID, CIDR: "10.0.1.0/24", Zone: "us-east-1a"}
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Put(key(scope, kindDefaults, "vpc"), []byte("{}"))
		if err := storage.Write(tx, key(scope, kindSubnet, sn.ID), sn); err != nil {
			return err
		}

		return storage.Write(tx, key(scope, kindVPC, old.ID), old)
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := s.DescribeRouteTables(ctx, &api.DescribeRouteTablesRequest{})
	if err != nil || len(out.RouteTables) != 1 || *out.RouteTables[0].VpcId != old.ID ||
		!*out.RouteTables[0].Associations[0].Main || *out.RouteTables[0].Routes[0].DestinationCidrBlock != old.CIDR {
		t.Fatalf("got %+v, %v", out, err)
	}

	acls, err := s.DescribeNetworkAcls(ctx, &api.DescribeNetworkAclsRequest{})
	if err != nil || len(acls.NetworkAcls) != 1 || !*acls.NetworkAcls[0].IsDefault ||
		len(acls.NetworkAcls[0].Associations) != 1 || *acls.NetworkAcls[0].Associations[0].SubnetId != sn.ID {
		t.Fatalf("got %+v, %v", acls, err)
	}
}
