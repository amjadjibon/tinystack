package integration_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/s3files"
	"github.com/aws/aws-sdk-go-v2/service/s3files/types"
)

const s3filesBucket = "lake"

func s3filesClient(s *httptest.Server, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds("test"), Retryer: aws.NopRetryer{}},
	)
}

// s3filesEnv holds the S3, IAM and EC2 resources a file system refers to.
type s3filesEnv struct {
	client                  *sdk.Client
	ec2                     *ec2sdk.Client
	role                    string
	vpc, subnetA, subnetB   string
	subnetV6, otherSubnet   string
	groupA, groupB, foreign string
}

func s3filesSetup(t *testing.T, s *httptest.Server) *s3filesEnv {
	t.Helper()
	ctx := context.Background()
	buckets := s3Client(s, "test", "us-east-1")
	if _, err := buckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new(s3filesBucket)}); err != nil {
		t.Fatal(err)
	}

	if _, err := buckets.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  new(s3filesBucket),
		VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatal(err)
	}

	role, err := iamClient(s, "test", "us-east-1").CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: new("s3files"),
		AssumeRolePolicyDocument: new(
			`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
				`"Principal":{"Service":"elasticfilesystem.amazonaws.com"},"Action":"sts:AssumeRole"}]}`,
		),
	})
	if err != nil {
		t.Fatal(err)
	}

	network := ec2Client(s, "test", "us-east-1")
	vpc, err := network.CreateVpc(ctx, &ec2sdk.CreateVpcInput{
		CidrBlock: new("10.30.0.0/16"), AmazonProvidedIpv6CidrBlock: new(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	prefix := netip.MustParsePrefix(*vpc.Vpc.Ipv6CidrBlockAssociationSet[0].Ipv6CidrBlock)
	subnet := func(vpcID, block, zone, ipv6 string) string {
		t.Helper()
		in := &ec2sdk.CreateSubnetInput{VpcId: new(vpcID), CidrBlock: new(block), AvailabilityZone: new(zone)}
		if ipv6 != "" {
			in.Ipv6CidrBlock = new(ipv6)
		}

		out, e := network.CreateSubnet(ctx, in)
		if e != nil {
			t.Fatal(e)
		}

		return *out.Subnet.SubnetId
	}

	group := func(vpcID, name string) string {
		t.Helper()
		out, e := network.CreateSecurityGroup(ctx, &ec2sdk.CreateSecurityGroupInput{
			VpcId: new(vpcID), GroupName: new(name), Description: new(name),
		})
		if e != nil {
			t.Fatal(e)
		}

		return *out.GroupId
	}

	other := createVPC(t, network, "10.40.0.0/16")
	return &s3filesEnv{
		client:      s3filesClient(s, "us-east-1"),
		ec2:         network,
		role:        *role.Role.Arn,
		vpc:         *vpc.Vpc.VpcId,
		subnetA:     subnet(*vpc.Vpc.VpcId, "10.30.1.0/24", "us-east-1a", ""),
		subnetB:     subnet(*vpc.Vpc.VpcId, "10.30.2.0/24", "us-east-1b", ""),
		subnetV6:    subnet(*vpc.Vpc.VpcId, "10.30.3.0/24", "us-east-1c", netip.PrefixFrom(prefix.Addr(), 64).String()),
		otherSubnet: subnet(other, "10.40.1.0/24", "us-east-1a", ""),
		groupA:      group(*vpc.Vpc.VpcId, "s3files-a"),
		groupB:      group(*vpc.Vpc.VpcId, "s3files-b"),
		foreign:     group(other, "s3files-foreign"),
	}
}

func (e *s3filesEnv) s3filesCreateFileSystem(
	t *testing.T,
	token string,
	tags ...types.Tag,
) *sdk.CreateFileSystemOutput {
	t.Helper()
	out, err := e.client.CreateFileSystem(context.Background(), &sdk.CreateFileSystemInput{
		Bucket: new("arn:aws:s3:::" + s3filesBucket), RoleArn: new(e.role), ClientToken: new(token), Tags: tags,
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

func s3filesFileSystemState(t *testing.T, c *sdk.Client, id *string, want types.LifeCycleState) {
	t.Helper()
	waitFor(t, "file system "+*id+" to be "+string(want), func() bool {
		out, err := c.GetFileSystem(context.Background(), &sdk.GetFileSystemInput{FileSystemId: id})
		if want == "" {
			if err != nil {
				wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
			}

			return err != nil
		}

		if err != nil {
			t.Fatal(err)
		}

		return out.Status == want
	})
}

func s3filesMountTargetState(t *testing.T, c *sdk.Client, id *string, want types.LifeCycleState) {
	t.Helper()
	waitFor(t, "mount target "+*id+" to be "+string(want), func() bool {
		out, err := c.GetMountTarget(context.Background(), &sdk.GetMountTargetInput{MountTargetId: id})
		if want == "" {
			if err != nil {
				wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
			}

			return err != nil
		}

		if err != nil {
			t.Fatal(err)
		}

		return out.Status == want
	})
}

func s3filesAccessPointState(t *testing.T, c *sdk.Client, id *string, want types.LifeCycleState) {
	t.Helper()
	waitFor(t, "access point "+*id+" to be "+string(want), func() bool {
		out, err := c.GetAccessPoint(context.Background(), &sdk.GetAccessPointInput{AccessPointId: id})
		if want == "" {
			if err != nil {
				wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
			}

			return err != nil
		}

		if err != nil {
			t.Fatal(err)
		}

		return out.Status == want
	})
}

func TestS3FilesFileSystemLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := server(t)
	env := s3filesSetup(t, s)
	c := env.client
	bucket := "arn:aws:s3:::" + s3filesBucket

	made, err := c.CreateFileSystem(ctx, &sdk.CreateFileSystemInput{
		Bucket: new(bucket), RoleArn: new(env.role), ClientToken: new("one"), Prefix: new("data/"),
		Tags: []types.Tag{{Key: new("Name"), Value: new("lake")}, {Key: new("team"), Value: new("core")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if made.Status != types.LifeCycleStateCreating || *made.Bucket != bucket || *made.Prefix != "data/" ||
		*made.Name != "lake" || *made.OwnerId == "" || *made.RoleArn != env.role || len(made.Tags) != 2 ||
		*made.FileSystemArn != "arn:aws:s3files:us-east-1:"+*made.OwnerId+":file-system/"+*made.FileSystemId ||
		made.CreationTime == nil || *made.ClientToken != "one" {
		t.Fatalf("create: %+v", made)
	}

	again, err := c.CreateFileSystem(ctx, &sdk.CreateFileSystemInput{
		Bucket: new(bucket), RoleArn: new(env.role), ClientToken: new("one"), Prefix: new("data/"),
		Tags: []types.Tag{{Key: new("team"), Value: new("core")}, {Key: new("Name"), Value: new("lake")}},
	})
	if err != nil || *again.FileSystemId != *made.FileSystemId {
		t.Fatalf("replay: %+v %v", again, err)
	}

	_, err = c.CreateFileSystem(ctx, &sdk.CreateFileSystemInput{
		Bucket: new(bucket), RoleArn: new(env.role), ClientToken: new("one"), Prefix: new("other/"),
	})
	wantAccessAnalyzerError(t, err, "ConflictException")
	s3filesFileSystemState(t, c, made.FileSystemId, types.LifeCycleStateAvailable)

	byARN, err := c.GetFileSystem(ctx, &sdk.GetFileSystemInput{FileSystemId: made.FileSystemArn})
	if err != nil || *byARN.FileSystemId != *made.FileSystemId || byARN.Status != types.LifeCycleStateAvailable {
		t.Fatalf("get by ARN: %+v %v", byARN, err)
	}

	// References must exist, and the role must belong to the caller's account.
	for name, in := range map[string]*sdk.CreateFileSystemInput{
		"missing bucket": {Bucket: new("arn:aws:s3:::absent"), RoleArn: new(env.role)},
		"missing role":   {Bucket: new(bucket), RoleArn: new("arn:aws:iam::000000000000:role/absent")},
		"missing key": {
			Bucket: new(bucket), RoleArn: new(env.role),
			KmsKeyId: new("arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000000"),
		},
	} {
		_, err = c.CreateFileSystem(ctx, in)
		if err == nil {
			t.Fatalf("%s: created", name)
		}

		wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	}

	for name, in := range map[string]*sdk.CreateFileSystemInput{
		"object ARN":   {Bucket: new(bucket + "/key"), RoleArn: new(env.role)},
		"foreign role": {Bucket: new(bucket), RoleArn: new("arn:aws:iam::111122223333:role/s3files")},
		"reserved tag": {
			Bucket:  new(bucket),
			RoleArn: new(env.role),
			Tags: []types.Tag{{
				Key:   new("aws:x"),
				Value: new("v"),
			}},
		},
		"repeated tag": {
			Bucket:  new(bucket),
			RoleArn: new(env.role),
			Tags: []types.Tag{
				{
					Key:   new("a"),
					Value: new("1"),
				},
				{
					Key:   new("a"),
					Value: new("2"),
				},
			},
		},
		"bad prefix":      {Bucket: new(bucket), RoleArn: new(env.role), Prefix: new("no-slash")},
		"role not an ARN": {Bucket: new(bucket), RoleArn: new("s3files")},
	} {
		_, err = c.CreateFileSystem(ctx, in)
		if err == nil {
			t.Fatalf("%s: created", name)
		}

		wantAccessAnalyzerError(t, err, "ValidationException")
	}

	key, err := kmsClient(s, "test", "us-east-1").CreateKey(ctx, &kms.CreateKeyInput{})
	if err != nil {
		t.Fatal(err)
	}

	encrypted, err := c.CreateFileSystem(ctx, &sdk.CreateFileSystemInput{
		Bucket: new(bucket), RoleArn: new(env.role), KmsKeyId: key.KeyMetadata.Arn,
	})
	if err != nil || *encrypted.KmsKeyId != *key.KeyMetadata.Arn || *encrypted.ClientToken == "" {
		t.Fatalf("kms: %+v %v", encrypted, err)
	}

	// Versioning is a prerequisite, even when bucket warnings are acknowledged.
	if _, err = s3Client(
		s,
		"test",
		"us-east-1",
	).CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("plain")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateFileSystem(
		ctx,
		&sdk.CreateFileSystemInput{Bucket: new("arn:aws:s3:::plain"), RoleArn: new(env.role)},
	)
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = c.CreateFileSystem(ctx, &sdk.CreateFileSystemInput{
		Bucket: new("arn:aws:s3:::plain"), RoleArn: new(env.role), AcceptBucketWarning: new(true),
	})
	wantAccessAnalyzerError(t, err, "ValidationException")
	if _, err = s3Client(s, "test", "us-east-1").PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  new("plain"),
		VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatal(err)
	}

	plain, err := c.CreateFileSystem(ctx, &sdk.CreateFileSystemInput{
		Bucket: new("arn:aws:s3:::plain"), RoleArn: new(env.role), AcceptBucketWarning: new(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Listing is in ID order, filters by bucket and pages.
	listed, err := c.ListFileSystems(ctx, &sdk.ListFileSystemsInput{})
	if err != nil || len(listed.FileSystems) != 3 || listed.NextToken != nil {
		t.Fatalf("list: %+v %v", listed, err)
	}

	ids := []string{}
	for _, f := range listed.FileSystems {
		ids = append(ids, *f.FileSystemId)
		if *f.OwnerId == "" || *f.RoleArn != env.role || f.CreationTime == nil {
			t.Fatalf("list entry: %+v", f)
		}
	}

	if !slices.IsSorted(ids) {
		t.Fatalf("not in ID order: %v", ids)
	}

	var got []string
	var token *string
	for pages := 0; ; pages++ {
		page, e := c.ListFileSystems(ctx, &sdk.ListFileSystemsInput{MaxResults: new(int32(2)), NextToken: token})
		if e != nil {
			t.Fatal(e)
		}

		for _, f := range page.FileSystems {
			got = append(got, *f.FileSystemId)
		}

		if token = page.NextToken; token == nil {
			if pages != 1 {
				t.Fatalf("pages: %d", pages+1)
			}

			break
		}
	}

	if !slices.Equal(got, ids) {
		t.Fatalf("paged %v, want %v", got, ids)
	}

	filtered, err := c.ListFileSystems(ctx, &sdk.ListFileSystemsInput{Bucket: new("arn:aws:s3:::plain")})
	if err != nil || len(filtered.FileSystems) != 1 || *filtered.FileSystems[0].FileSystemId != *plain.FileSystemId {
		t.Fatalf("filter: %+v %v", filtered, err)
	}

	_, err = c.ListFileSystems(ctx, &sdk.ListFileSystemsInput{NextToken: new("!")})
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = c.ListFileSystems(ctx, &sdk.ListFileSystemsInput{MaxResults: new(int32(101))})
	wantAccessAnalyzerError(t, err, "ValidationException")

	// A file system with a mount target or an access point cannot be deleted.
	s3filesFileSystemState(t, c, plain.FileSystemId, types.LifeCycleStateAvailable)
	mt, err := c.CreateMountTarget(
		ctx,
		&sdk.CreateMountTargetInput{FileSystemId: plain.FileSystemId, SubnetId: new(env.subnetA)},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteFileSystem(ctx, &sdk.DeleteFileSystemInput{FileSystemId: plain.FileSystemId})
	wantAccessAnalyzerError(t, err, "ConflictException")
	if _, err = c.DeleteMountTarget(ctx, &sdk.DeleteMountTargetInput{MountTargetId: mt.MountTargetId}); err != nil {
		t.Fatal(err)
	}

	s3filesMountTargetState(t, c, mt.MountTargetId, "")
	ap, err := c.CreateAccessPoint(ctx, &sdk.CreateAccessPointInput{FileSystemId: plain.FileSystemId})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteFileSystem(ctx, &sdk.DeleteFileSystemInput{FileSystemId: plain.FileSystemId})
	wantAccessAnalyzerError(t, err, "ConflictException")
	if _, err = c.DeleteAccessPoint(ctx, &sdk.DeleteAccessPointInput{AccessPointId: ap.AccessPointId}); err != nil {
		t.Fatal(err)
	}

	s3filesAccessPointState(t, c, ap.AccessPointId, "")
	if _, err = c.DeleteFileSystem(ctx, &sdk.DeleteFileSystemInput{
		FileSystemId: plain.FileSystemId, ForceDelete: new(true),
	}); err != nil {
		t.Fatal(err)
	}

	gone, err := c.GetFileSystem(ctx, &sdk.GetFileSystemInput{FileSystemId: plain.FileSystemId})
	if err == nil && gone.Status != types.LifeCycleStateDeleting {
		t.Fatalf("deleting: %+v", gone)
	}

	s3filesFileSystemState(t, c, plain.FileSystemId, "")
	_, err = c.DeleteFileSystem(ctx, &sdk.DeleteFileSystemInput{FileSystemId: plain.FileSystemId})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.GetFileSystem(ctx, &sdk.GetFileSystemInput{FileSystemId: new("fs-00000000000000000")})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.GetFileSystem(ctx, &sdk.GetFileSystemInput{FileSystemId: new("not-an-id")})
	wantAccessAnalyzerError(t, err, "ValidationException")

	foreignBuckets := s3Client(s, "111122223333", "us-east-1")
	if _, err = foreignBuckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("foreign-bucket")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateFileSystem(ctx, &sdk.CreateFileSystemInput{
		Bucket: new("arn:aws:s3:::foreign-bucket"), RoleArn: new(env.role),
	})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")

	// Resources are scoped to their Region.
	east2 := s3filesClient(s, "us-east-2")
	_, err = east2.CreateFileSystem(ctx, &sdk.CreateFileSystemInput{Bucket: new(bucket), RoleArn: new(env.role)})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")

	_, err = east2.GetFileSystem(ctx, &sdk.GetFileSystemInput{FileSystemId: made.FileSystemId})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = east2.GetFileSystem(ctx, &sdk.GetFileSystemInput{FileSystemId: made.FileSystemArn})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
}

func TestS3FilesMountTargets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := s3filesSetup(t, server(t))
	c := env.client
	fs := env.s3filesCreateFileSystem(t, "mounts")
	other := env.s3filesCreateFileSystem(t, "other")

	mt, err := c.CreateMountTarget(
		ctx,
		&sdk.CreateMountTargetInput{FileSystemId: fs.FileSystemArn, SubnetId: new(env.subnetA)},
	)
	if err != nil {
		t.Fatal(err)
	}

	if mt.Status != types.LifeCycleStateCreating || *mt.VpcId != env.vpc || *mt.FileSystemId != *fs.FileSystemId ||
		*mt.SubnetId != env.subnetA || *mt.Ipv4Address != "10.30.1.4" || mt.Ipv6Address != nil ||
		*mt.AvailabilityZoneId == "" || len(mt.SecurityGroups) != 1 || *mt.OwnerId == "" {
		t.Fatalf("create: %+v", mt)
	}

	defaultGroup, err := env.ec2.DescribeSecurityGroups(ctx, &ec2sdk.DescribeSecurityGroupsInput{
		GroupIds: mt.SecurityGroups,
	})
	if err != nil || *defaultGroup.SecurityGroups[0].GroupName != "default" ||
		*defaultGroup.SecurityGroups[0].VpcId != env.vpc {
		t.Fatalf("default group: %+v %v", defaultGroup, err)
	}

	s3filesMountTargetState(t, c, mt.MountTargetId, types.LifeCycleStateAvailable)

	// The limit is per AZ, including distinct subnets in that AZ.
	sameZone, err := env.ec2.CreateSubnet(ctx, &ec2sdk.CreateSubnetInput{
		VpcId: new(env.vpc), CidrBlock: new("10.30.4.0/24"), AvailabilityZone: new("us-east-1a"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateMountTarget(ctx, &sdk.CreateMountTargetInput{
		FileSystemId: fs.FileSystemId, SubnetId: sameZone.Subnet.SubnetId,
	})
	wantAccessAnalyzerError(t, err, "ConflictException")

	// Another file system's mount target in the same subnet gets the next address.
	neighbor, err := c.CreateMountTarget(
		ctx,
		&sdk.CreateMountTargetInput{FileSystemId: other.FileSystemId, SubnetId: new(env.subnetA)},
	)
	if err != nil || *neighbor.Ipv4Address != "10.30.1.5" {
		t.Fatalf("neighbor: %+v %v", neighbor, err)
	}

	for name, tc := range map[string]struct {
		in   *sdk.CreateMountTargetInput
		code string
	}{
		"one per zone": {
			&sdk.CreateMountTargetInput{
				FileSystemId: fs.FileSystemId,
				SubnetId:     new(env.subnetA),
			},
			"ConflictException",
		},
		"other VPC": {
			&sdk.CreateMountTargetInput{FileSystemId: fs.FileSystemId, SubnetId: new(env.otherSubnet)}, "ConflictException",
		},
		"foreign group": {
			&sdk.CreateMountTargetInput{
				FileSystemId:   fs.FileSystemId,
				SubnetId:       new(env.subnetB),
				SecurityGroups: []string{env.foreign},
			},
			"ValidationException",
		},
		"missing group": {
			&sdk.CreateMountTargetInput{
				FileSystemId:   fs.FileSystemId,
				SubnetId:       new(env.subnetB),
				SecurityGroups: []string{"sg-0123456789abcdef0"},
			},
			"ResourceNotFoundException",
		},
		"repeated group": {
			&sdk.CreateMountTargetInput{
				FileSystemId: fs.FileSystemId,
				SubnetId:     new(env.subnetB),
				SecurityGroups: []string{
					env.groupA,
					env.groupA,
				},
			},
			"ValidationException",
		},
		"missing subnet": {
			&sdk.CreateMountTargetInput{FileSystemId: fs.FileSystemId, SubnetId: new("subnet-0123456789abcdef0")},
			"ResourceNotFoundException",
		},
		"missing file system": {
			&sdk.CreateMountTargetInput{FileSystemId: new("fs-00000000000000000"), SubnetId: new(env.subnetB)},
			"ResourceNotFoundException",
		},
		"address outside subnet": {
			&sdk.CreateMountTargetInput{
				FileSystemId: fs.FileSystemId,
				SubnetId:     new(env.subnetB),
				Ipv4Address:  new("10.99.0.9"),
			},
			"ValidationException",
		},
		"reserved address": {
			&sdk.CreateMountTargetInput{
				FileSystemId: fs.FileSystemId,
				SubnetId:     new(env.subnetB),
				Ipv4Address:  new("10.30.2.1"),
			},
			"ValidationException",
		},
		"address in use": {
			&sdk.CreateMountTargetInput{
				FileSystemId: other.FileSystemId,
				SubnetId:     new(env.subnetB),
				Ipv4Address:  new("10.30.1.4"),
			},
			"ValidationException",
		},
		"not an address": {
			&sdk.CreateMountTargetInput{
				FileSystemId: fs.FileSystemId,
				SubnetId:     new(env.subnetB),
				Ipv4Address:  new("999.1.1.1"),
			},
			"ValidationException",
		},
		"ipv6 on ipv4 only": {
			&sdk.CreateMountTargetInput{FileSystemId: fs.FileSystemId, SubnetId: new(env.subnetB), Ipv6Address: new("2600::1")},
			"ValidationException",
		},
		"ipv4 on ipv6 only": {
			&sdk.CreateMountTargetInput{
				FileSystemId: fs.FileSystemId, SubnetId: new(env.subnetV6), IpAddressType: types.IpAddressTypeIpv6Only,
				Ipv4Address: new("10.30.3.9"),
			},
			"ValidationException",
		},
		"ipv6 without subnet CIDR": {
			&sdk.CreateMountTargetInput{
				FileSystemId:  fs.FileSystemId,
				SubnetId:      new(env.subnetB),
				IpAddressType: types.IpAddressTypeDualStack,
			},
			"ValidationException",
		},
		"bad address type": {
			&sdk.CreateMountTargetInput{FileSystemId: fs.FileSystemId, SubnetId: new(env.subnetB), IpAddressType: "IPV5"},
			"ValidationException",
		},
	} {
		_, err = c.CreateMountTarget(ctx, tc.in)
		if err == nil {
			t.Fatalf("%s: created", name)
		}

		t.Logf("%s: %v", name, err)
		wantAccessAnalyzerError(t, err, tc.code)
	}

	// The address "in use" case above is a conflict: it must name a different code than a malformed address.
	_, err = c.CreateMountTarget(ctx, &sdk.CreateMountTargetInput{
		FileSystemId:   fs.FileSystemId,
		SubnetId:       new(env.subnetB),
		Ipv4Address:    new("10.30.2.9"),
		SecurityGroups: []string{env.groupB, env.groupA},
	})
	if err != nil {
		t.Fatal(err)
	}

	dual, err := c.CreateMountTarget(ctx, &sdk.CreateMountTargetInput{
		FileSystemId: fs.FileSystemId, SubnetId: new(env.subnetV6), IpAddressType: types.IpAddressTypeDualStack,
	})
	if err != nil || *dual.Ipv4Address != "10.30.3.4" || netip.MustParseAddr(*dual.Ipv6Address).Is4() {
		t.Fatalf("dual stack: %+v %v", dual, err)
	}

	if _, err = c.DeleteMountTarget(ctx, &sdk.DeleteMountTargetInput{MountTargetId: dual.MountTargetId}); err != nil {
		t.Fatal(err)
	}

	s3filesMountTargetState(t, c, dual.MountTargetId, "")
	v6only, err := c.CreateMountTarget(ctx, &sdk.CreateMountTargetInput{
		FileSystemId: fs.FileSystemId, SubnetId: new(env.subnetV6), IpAddressType: types.IpAddressTypeIpv6Only,
	})
	if err != nil || v6only.Ipv4Address != nil || v6only.Ipv6Address == nil {
		t.Fatalf("ipv6 only: %+v %v", v6only, err)
	}

	got, err := c.GetMountTarget(ctx, &sdk.GetMountTargetInput{MountTargetId: mt.MountTargetId})
	if err != nil || *got.MountTargetId != *mt.MountTargetId || *got.Ipv4Address != "10.30.1.4" {
		t.Fatalf("get: %+v %v", got, err)
	}

	updated, err := c.UpdateMountTarget(ctx, &sdk.UpdateMountTargetInput{
		MountTargetId: mt.MountTargetId, SecurityGroups: []string{env.groupB, env.groupA},
	})
	expectedGroups := []string{env.groupA, env.groupB}
	slices.Sort(expectedGroups)
	if err != nil || !slices.Equal(updated.SecurityGroups, expectedGroups) {
		t.Fatalf("update: %+v %v", updated, err)
	}

	for name, in := range map[string]*sdk.UpdateMountTargetInput{
		"foreign group":   {MountTargetId: mt.MountTargetId, SecurityGroups: []string{env.foreign}},
		"repeated group":  {MountTargetId: mt.MountTargetId, SecurityGroups: []string{env.groupA, env.groupA}},
		"missing target":  {MountTargetId: new("fsmt-00000000000000000"), SecurityGroups: []string{env.groupA}},
		"malformed group": {MountTargetId: mt.MountTargetId, SecurityGroups: []string{"nope"}},
	} {
		if _, err = c.UpdateMountTarget(ctx, in); err == nil {
			t.Fatalf("%s: updated", name)
		}
	}

	_, err = c.GetMountTarget(ctx, &sdk.GetMountTargetInput{MountTargetId: new("fsmt-00000000000000000")})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.DeleteMountTarget(ctx, &sdk.DeleteMountTargetInput{MountTargetId: new("fsmt-00000000000000000")})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")

	// Listing filters by file system and by the access point's file system, in ID order, and pages.
	ap, err := c.CreateAccessPoint(ctx, &sdk.CreateAccessPointInput{FileSystemId: fs.FileSystemId})
	if err != nil {
		t.Fatal(err)
	}

	all, err := c.ListMountTargets(ctx, &sdk.ListMountTargetsInput{})
	if err != nil || len(all.MountTargets) != 4 {
		t.Fatalf("list all: %+v %v", all, err)
	}

	ofFS, err := c.ListMountTargets(ctx, &sdk.ListMountTargetsInput{FileSystemId: fs.FileSystemArn})
	if err != nil || len(ofFS.MountTargets) != 3 {
		t.Fatalf("list by file system: %+v %v", ofFS, err)
	}

	ofAP, err := c.ListMountTargets(ctx, &sdk.ListMountTargetsInput{AccessPointId: ap.AccessPointArn})
	if err != nil || len(ofAP.MountTargets) != 3 {
		t.Fatalf("list by access point: %+v %v", ofAP, err)
	}

	var ids, paged []string
	for _, m := range ofFS.MountTargets {
		ids = append(ids, *m.MountTargetId)
		if *m.VpcId != env.vpc || *m.OwnerId == "" || *m.FileSystemId != *fs.FileSystemId {
			t.Fatalf("list entry: %+v", m)
		}
	}

	if !slices.IsSorted(ids) {
		t.Fatalf("not in ID order: %v", ids)
	}

	var token *string
	for {
		page, e := c.ListMountTargets(ctx, &sdk.ListMountTargetsInput{
			FileSystemId: fs.FileSystemId, MaxResults: new(int32(2)), NextToken: token,
		})
		if e != nil {
			t.Fatal(e)
		}

		for _, m := range page.MountTargets {
			paged = append(paged, *m.MountTargetId)
		}

		if token = page.NextToken; token == nil {
			break
		}
	}

	if !slices.Equal(paged, ids) {
		t.Fatalf("paged %v, want %v", paged, ids)
	}

	_, err = c.ListMountTargets(ctx, &sdk.ListMountTargetsInput{FileSystemId: new("fs-00000000000000000")})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.ListMountTargets(ctx, &sdk.ListMountTargetsInput{AccessPointId: new("fsap-00000000000000000")})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.ListMountTargets(ctx, &sdk.ListMountTargetsInput{
		FileSystemId: other.FileSystemId, AccessPointId: ap.AccessPointId,
	})
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = c.ListMountTargets(ctx, &sdk.ListMountTargetsInput{NextToken: new("!")})
	wantAccessAnalyzerError(t, err, "ValidationException")

	// A deleted mount target frees its zone and its address.
	if _, err = c.DeleteMountTarget(ctx, &sdk.DeleteMountTargetInput{MountTargetId: mt.MountTargetId}); err != nil {
		t.Fatal(err)
	}

	s3filesMountTargetState(t, c, mt.MountTargetId, "")
	again, err := c.CreateMountTarget(ctx, &sdk.CreateMountTargetInput{
		FileSystemId: fs.FileSystemId, SubnetId: new(env.subnetA), Ipv4Address: new("10.30.1.4"),
	})
	if err != nil || *again.Ipv4Address != "10.30.1.4" {
		t.Fatalf("reuse: %+v %v", again, err)
	}
}

func TestS3FilesAccessPoints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := s3filesSetup(t, server(t))
	c := env.client
	fs := env.s3filesCreateFileSystem(t, "points")
	other := env.s3filesCreateFileSystem(t, "other")

	in := &sdk.CreateAccessPointInput{
		FileSystemId: fs.FileSystemArn,
		ClientToken:  new("ap"),
		PosixUser: &types.PosixUser{
			Uid:           new(int64(1000)),
			Gid:           new(int64(1000)),
			SecondaryGids: []int64{2000, 3000},
		},
		RootDirectory: &types.RootDirectory{
			Path: new("/app/data"),
			CreationPermissions: &types.CreationPermissions{
				OwnerUid: new(int64(1000)), OwnerGid: new(int64(1000)), Permissions: new("0755"),
			},
		},
		Tags: []types.Tag{{Key: new("Name"), Value: new("app")}},
	}

	made, err := c.CreateAccessPoint(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	if made.Status != types.LifeCycleStateCreating || *made.Name != "app" || *made.ClientToken != "ap" ||
		*made.FileSystemId != *fs.FileSystemId || *made.OwnerId == "" ||
		*made.AccessPointArn != *fs.FileSystemArn+"/access-point/"+*made.AccessPointId ||
		*made.RootDirectory.Path != "/app/data" || *made.PosixUser.Uid != 1000 ||
		!slices.Equal(made.PosixUser.SecondaryGids, []int64{2000, 3000}) ||
		*made.RootDirectory.CreationPermissions.Permissions != "0755" {
		t.Fatalf("create: %+v", made)
	}

	again, err := c.CreateAccessPoint(ctx, in)
	if err != nil || *again.AccessPointId != *made.AccessPointId {
		t.Fatalf("replay: %+v %v", again, err)
	}

	// AWS ignores a reused access-point token, even when its parameters differ.
	for _, parent := range []*string{fs.FileSystemId, other.FileSystemId} {
		replayed, replayErr := c.CreateAccessPoint(ctx, &sdk.CreateAccessPointInput{
			FileSystemId: parent, ClientToken: new("ap"),
		})
		if replayErr != nil || *replayed.AccessPointId != *made.AccessPointId ||
			*replayed.FileSystemId != *fs.FileSystemId {
			t.Fatalf("changed-parameter replay: %+v %v", replayed, replayErr)
		}
	}

	for name, in := range map[string]*sdk.CreateAccessPointInput{
		"bad permissions": {
			FileSystemId: fs.FileSystemId,
			RootDirectory: &types.RootDirectory{CreationPermissions: &types.CreationPermissions{
				OwnerUid: new(int64(1)), OwnerGid: new(int64(1)), Permissions: new("999"),
			}},
		},
		"bad path":    {FileSystemId: fs.FileSystemId, RootDirectory: &types.RootDirectory{Path: new("relative")}},
		"hidden path": {FileSystemId: fs.FileSystemId, RootDirectory: &types.RootDirectory{Path: new("/.hidden")}},
		"deep path":   {FileSystemId: fs.FileSystemId, RootDirectory: &types.RootDirectory{Path: new("/a/b/c/d/e")}},
		"negative uid": {
			FileSystemId: fs.FileSystemId, PosixUser: &types.PosixUser{Uid: new(int64(-1)), Gid: new(int64(0))},
		},
	} {
		_, err = c.CreateAccessPoint(ctx, in)
		if err == nil {
			t.Fatalf("%s: created", name)
		}

		wantAccessAnalyzerError(t, err, "ValidationException")
	}

	_, err = c.CreateAccessPoint(ctx, &sdk.CreateAccessPointInput{FileSystemId: new("fs-00000000000000000")})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")

	root, err := c.CreateAccessPoint(ctx, &sdk.CreateAccessPointInput{
		FileSystemId: fs.FileSystemId, RootDirectory: &types.RootDirectory{Path: new("/")},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetAccessPoint(ctx, &sdk.GetAccessPointInput{
		AccessPointId: new(*other.FileSystemArn + "/access-point/" + *made.AccessPointId),
	})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")

	listed, err := c.ListAccessPoints(ctx, &sdk.ListAccessPointsInput{FileSystemId: fs.FileSystemArn})
	if err != nil || len(listed.AccessPoints) != 2 {
		t.Fatalf("list: %+v %v", listed, err)
	}

	ids := []string{*listed.AccessPoints[0].AccessPointId, *listed.AccessPoints[1].AccessPointId}
	want := []string{*made.AccessPointId, *root.AccessPointId}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Fatalf("order %v, want %v", ids, want)
	}

	first, err := c.ListAccessPoints(
		ctx,
		&sdk.ListAccessPointsInput{FileSystemId: fs.FileSystemId, MaxResults: new(int32(1))},
	)
	if err != nil || len(first.AccessPoints) != 1 || first.NextToken == nil {
		t.Fatalf("first page: %+v %v", first, err)
	}

	second, err := c.ListAccessPoints(ctx, &sdk.ListAccessPointsInput{
		FileSystemId: fs.FileSystemId, MaxResults: new(int32(1)), NextToken: first.NextToken,
	})
	if err != nil || len(second.AccessPoints) != 1 || second.NextToken != nil ||
		*second.AccessPoints[0].AccessPointId != want[1] {
		t.Fatalf("second page: %+v %v", second, err)
	}

	_, err = c.ListAccessPoints(ctx, &sdk.ListAccessPointsInput{FileSystemId: new("fs-00000000000000000")})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.ListAccessPoints(ctx, &sdk.ListAccessPointsInput{FileSystemId: fs.FileSystemId, NextToken: new("!")})
	wantAccessAnalyzerError(t, err, "ValidationException")

	if _, err = c.DeleteAccessPoint(ctx, &sdk.DeleteAccessPointInput{AccessPointId: made.AccessPointArn}); err != nil {
		t.Fatal(err)
	}

	s3filesAccessPointState(t, c, made.AccessPointId, "")
	_, err = c.DeleteAccessPoint(ctx, &sdk.DeleteAccessPointInput{AccessPointId: made.AccessPointId})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.GetAccessPoint(ctx, &sdk.GetAccessPointInput{AccessPointId: new("nope")})
	wantAccessAnalyzerError(t, err, "ValidationException")
}

func TestS3FilesPolicyAndSynchronization(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := s3filesSetup(t, server(t))
	c := env.client
	fs := env.s3filesCreateFileSystem(t, "config")

	_, err := c.GetFileSystemPolicy(ctx, &sdk.GetFileSystemPolicyInput{FileSystemId: fs.FileSystemId})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.DeleteFileSystemPolicy(ctx, &sdk.DeleteFileSystemPolicyInput{FileSystemId: fs.FileSystemId})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	for _, bad := range []string{"not json", "[]", "null", `{"Version":"2012-10-17"}`} {
		_, err = c.PutFileSystemPolicy(
			ctx,
			&sdk.PutFileSystemPolicyInput{FileSystemId: fs.FileSystemId, Policy: new(bad)},
		)
		wantAccessAnalyzerError(t, err, "ValidationException")
	}

	_, err = c.PutFileSystemPolicy(
		ctx,
		&sdk.PutFileSystemPolicyInput{FileSystemId: new("fs-00000000000000000"), Policy: new(`{"Statement":[]}`)},
	)
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3files:ClientMount"}]}`
	if _, err = c.PutFileSystemPolicy(
		ctx,
		&sdk.PutFileSystemPolicyInput{FileSystemId: fs.FileSystemArn, Policy: new(policy)},
	); err != nil {
		t.Fatal(err)
	}

	got, err := c.GetFileSystemPolicy(ctx, &sdk.GetFileSystemPolicyInput{FileSystemId: fs.FileSystemId})
	if err != nil || *got.Policy != policy || *got.FileSystemId != *fs.FileSystemId {
		t.Fatalf("policy: %+v %v", got, err)
	}

	if _, err = c.DeleteFileSystemPolicy(
		ctx,
		&sdk.DeleteFileSystemPolicyInput{FileSystemId: fs.FileSystemId},
	); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetFileSystemPolicy(ctx, &sdk.GetFileSystemPolicyInput{FileSystemId: fs.FileSystemId})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")

	rules := func(days int32, prefix string) ([]types.ImportDataRule, []types.ExpirationDataRule) {
		return []types.ImportDataRule{{
				Prefix: new(prefix), Trigger: types.ImportTriggerOnFileAccess, SizeLessThan: new(int64(131072)),
			}},
			[]types.ExpirationDataRule{{DaysAfterLastAccess: new(days)}}
	}

	imports, expirations := rules(30, "")

	_, err = c.GetSynchronizationConfiguration(
		ctx,
		&sdk.GetSynchronizationConfigurationInput{FileSystemId: fs.FileSystemId},
	)
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.PutSynchronizationConfiguration(ctx, &sdk.PutSynchronizationConfigurationInput{
		FileSystemId: fs.FileSystemId, LatestVersionNumber: new(int32(1)),
		ImportDataRules: imports, ExpirationDataRules: expirations,
	})
	wantAccessAnalyzerError(t, err, "ConflictException")
	if _, err = c.PutSynchronizationConfiguration(ctx, &sdk.PutSynchronizationConfigurationInput{
		FileSystemId: fs.FileSystemArn, ImportDataRules: imports, ExpirationDataRules: expirations,
	}); err != nil {
		t.Fatal(err)
	}

	first, err := c.GetSynchronizationConfiguration(
		ctx,
		&sdk.GetSynchronizationConfigurationInput{FileSystemId: fs.FileSystemId},
	)
	if err != nil || *first.LatestVersionNumber != 1 || len(first.ImportDataRules) != 1 ||
		first.ImportDataRules[0].Trigger != types.ImportTriggerOnFileAccess ||
		*first.ImportDataRules[0].SizeLessThan != 131072 || *first.ExpirationDataRules[0].DaysAfterLastAccess != 30 {
		t.Fatalf("first: %+v %v", first, err)
	}

	imports, expirations = rules(60, "logs/")
	update := &sdk.PutSynchronizationConfigurationInput{
		FileSystemId: fs.FileSystemId, ImportDataRules: imports, ExpirationDataRules: expirations,
	}

	_, err = c.PutSynchronizationConfiguration(ctx, update)
	wantAccessAnalyzerError(t, err, "ConflictException")
	update.LatestVersionNumber = new(int32(7))
	_, err = c.PutSynchronizationConfiguration(ctx, update)
	wantAccessAnalyzerError(t, err, "ConflictException")
	update.LatestVersionNumber = new(int32(1))
	if _, err = c.PutSynchronizationConfiguration(ctx, update); err != nil {
		t.Fatal(err)
	}

	// The stale version is now rejected.
	_, err = c.PutSynchronizationConfiguration(ctx, update)
	wantAccessAnalyzerError(t, err, "ConflictException")
	second, err := c.GetSynchronizationConfiguration(
		ctx,
		&sdk.GetSynchronizationConfigurationInput{FileSystemId: fs.FileSystemId},
	)
	if err != nil || *second.LatestVersionNumber != 2 || *second.ImportDataRules[0].Prefix != "logs/" ||
		*second.ExpirationDataRules[0].DaysAfterLastAccess != 60 {
		t.Fatalf("second: %+v %v", second, err)
	}

	for name, in := range map[string]*sdk.PutSynchronizationConfigurationInput{
		"no import rules": {
			FileSystemId:        fs.FileSystemId,
			LatestVersionNumber: new(int32(2)),
			ImportDataRules:     []types.ImportDataRule{},
			ExpirationDataRules: expirations,
		},
		"two expiration rules": {
			FileSystemId: fs.FileSystemId, LatestVersionNumber: new(int32(2)), ImportDataRules: imports,
			ExpirationDataRules: append(slices.Clone(expirations), expirations...),
		},
		"expiration out of range": {
			FileSystemId: fs.FileSystemId, LatestVersionNumber: new(int32(2)), ImportDataRules: imports,
			ExpirationDataRules: []types.ExpirationDataRule{{DaysAfterLastAccess: new(int32(366))}},
		},
		"bad prefix": {
			FileSystemId: fs.FileSystemId, LatestVersionNumber: new(int32(2)), ExpirationDataRules: expirations,
			ImportDataRules: []types.ImportDataRule{{
				Prefix: new("logs"), Trigger: types.ImportTriggerOnFileAccess, SizeLessThan: new(int64(1)),
			}},
		},
		"bad trigger": {
			FileSystemId: fs.FileSystemId, LatestVersionNumber: new(int32(2)), ExpirationDataRules: expirations,
			ImportDataRules: []types.ImportDataRule{{Prefix: new(""), Trigger: "NEVER", SizeLessThan: new(int64(1))}},
		},
	} {
		_, err = c.PutSynchronizationConfiguration(ctx, in)
		if err == nil {
			t.Fatalf("%s: stored", name)
		}

		wantAccessAnalyzerError(t, err, "ValidationException")
	}

	_, err = c.GetSynchronizationConfiguration(
		ctx,
		&sdk.GetSynchronizationConfigurationInput{FileSystemId: new("fs-00000000000000000")},
	)
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
}

func TestS3FilesTags(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := server(t)
	env := s3filesSetup(t, s)
	c := env.client
	fs := env.s3filesCreateFileSystem(t, "tags", types.Tag{Key: new("team"), Value: new("core")})
	ap, err := c.CreateAccessPoint(ctx, &sdk.CreateAccessPointInput{
		FileSystemId: fs.FileSystemId, Tags: []types.Tag{{Key: new("Name"), Value: new("app")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	tags := func(resource *string) map[string]string {
		t.Helper()
		out, e := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceId: resource})
		if e != nil {
			t.Fatal(e)
		}

		got := map[string]string{}
		for _, tag := range out.Tags {
			got[*tag.Key] = *tag.Value
		}

		return got
	}

	if got := tags(fs.FileSystemId); len(got) != 1 || got["team"] != "core" {
		t.Fatalf("create tags: %v", got)
	}

	if _, err = c.TagResource(ctx, &sdk.TagResourceInput{
		ResourceId: fs.FileSystemArn,
		Tags:       []types.Tag{{Key: new("team"), Value: new("platform")}, {Key: new("env"), Value: new("dev")}},
	}); err != nil {
		t.Fatal(err)
	}

	if got := tags(fs.FileSystemId); len(got) != 2 || got["team"] != "platform" || got["env"] != "dev" {
		t.Fatalf("tagged: %v", got)
	}

	if _, err = c.TagResource(ctx, &sdk.TagResourceInput{
		ResourceId: ap.AccessPointArn, Tags: []types.Tag{{Key: new("env"), Value: new("test")}},
	}); err != nil {
		t.Fatal(err)
	}

	if got := tags(ap.AccessPointId); len(got) != 2 || got["env"] != "test" || got["Name"] != "app" {
		t.Fatalf("access point tags: %v", got)
	}

	renamed, err := c.GetAccessPoint(ctx, &sdk.GetAccessPointInput{AccessPointId: ap.AccessPointId})
	if err != nil || *renamed.Name != "app" || len(renamed.Tags) != 2 {
		t.Fatalf("access point: %+v %v", renamed, err)
	}

	// Tags page in key order.
	var keys []string
	var token *string
	for {
		page, e := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
			ResourceId: ap.AccessPointId, MaxResults: new(int32(1)), NextToken: token,
		})
		if e != nil {
			t.Fatal(e)
		}

		for _, tag := range page.Tags {
			keys = append(keys, *tag.Key)
		}

		if token = page.NextToken; token == nil {
			break
		}
	}

	if !slices.Equal(keys, []string{"Name", "env"}) {
		t.Fatalf("paged tags: %v", keys)
	}

	if _, err = c.UntagResource(
		ctx,
		&sdk.UntagResourceInput{ResourceId: fs.FileSystemId, TagKeys: []string{"env", "absent"}},
	); err != nil {
		t.Fatal(err)
	}

	if got := tags(fs.FileSystemId); len(got) != 1 || got["team"] != "platform" {
		t.Fatalf("untagged: %v", got)
	}

	_, err = c.TagResource(ctx, &sdk.TagResourceInput{
		ResourceId: fs.FileSystemId, Tags: []types.Tag{{Key: new("aws:reserved"), Value: new("x")}},
	})
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = c.TagResource(ctx, &sdk.TagResourceInput{
		ResourceId: fs.FileSystemId,
		Tags:       []types.Tag{{Key: new("a"), Value: new("1")}, {Key: new("a"), Value: new("2")}},
	})
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceId: fs.FileSystemId, Tags: []types.Tag{{Key: new("bad key!"), Value: new("x")}}},
	)
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = c.ListTagsForResource(
		ctx,
		&sdk.ListTagsForResourceInput{ResourceId: fs.FileSystemId, MaxResults: new(int32(51))},
	)
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceId: fs.FileSystemId, NextToken: new("!")})
	wantAccessAnalyzerError(t, err, "ValidationException")
	for _, missing := range []string{"fs-00000000000000000", "fsap-00000000000000000"} {
		_, err = c.TagResource(
			ctx,
			&sdk.TagResourceInput{ResourceId: new(missing), Tags: []types.Tag{{Key: new("a"), Value: new("1")}}},
		)
		wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
		_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceId: new(missing), TagKeys: []string{"a"}})
		wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
		_, err = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceId: new(missing)})
		wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	}

	// The Resource Groups Tagging API sees and changes the same tags.
	tagClient := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	found, err := tagClient.GetResources(ctx, &rgta.GetResourcesInput{ResourceTypeFilters: []string{"s3files"}})
	if err != nil || len(found.ResourceTagMappingList) != 2 {
		t.Fatalf("tagging API: %+v %v", found, err)
	}

	changed, err := tagClient.TagResources(ctx, &rgta.TagResourcesInput{
		ResourceARNList: []string{*fs.FileSystemArn, *ap.AccessPointArn}, Tags: map[string]string{"shared": "yes"},
	})
	if err != nil || len(changed.FailedResourcesMap) != 0 {
		t.Fatalf("tag resources: %+v %v", changed, err)
	}

	if got := tags(fs.FileSystemId); got["shared"] != "yes" {
		t.Fatalf("tagged through the tagging API: %v", got)
	}

	if got := tags(ap.AccessPointId); got["shared"] != "yes" {
		t.Fatalf("tagged through the tagging API: %v", got)
	}
}

func TestS3FilesSQLiteRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	t.Cleanup(func() { stop() })
	env := s3filesSetup(t, s)
	fs := env.s3filesCreateFileSystem(t, "persistent", types.Tag{Key: new("restart"), Value: new("yes")})
	mt, err := env.client.CreateMountTarget(ctx, &sdk.CreateMountTargetInput{
		FileSystemId: fs.FileSystemId, SubnetId: new(env.subnetA), SecurityGroups: []string{env.groupA},
	})
	if err != nil {
		t.Fatal(err)
	}

	ap, err := env.client.CreateAccessPoint(
		ctx,
		&sdk.CreateAccessPointInput{FileSystemId: fs.FileSystemId, ClientToken: new("ap")},
	)
	if err != nil {
		t.Fatal(err)
	}

	policy := `{"Statement":[]}`
	if _, err = env.client.PutFileSystemPolicy(
		ctx,
		&sdk.PutFileSystemPolicyInput{FileSystemId: fs.FileSystemId, Policy: new(policy)},
	); err != nil {
		t.Fatal(err)
	}

	imports := []types.ImportDataRule{
		{Prefix: new(""), Trigger: types.ImportTriggerOnDirectoryFirstAccess, SizeLessThan: new(int64(1))},
	}

	expirations := []types.ExpirationDataRule{{DaysAfterLastAccess: new(int32(7))}}
	if _, err = env.client.PutSynchronizationConfiguration(ctx, &sdk.PutSynchronizationConfigurationInput{
		FileSystemId: fs.FileSystemId, ImportDataRules: imports, ExpirationDataRules: expirations,
	}); err != nil {
		t.Fatal(err)
	}

	// Stop with deletions pending: the restarted worker finishes them.
	if _, err = env.client.DeleteMountTarget(
		ctx,
		&sdk.DeleteMountTargetInput{MountTargetId: mt.MountTargetId},
	); err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	c := s3filesClient(s, "us-east-1")
	s3filesMountTargetState(t, c, mt.MountTargetId, "")
	s3filesFileSystemState(t, c, fs.FileSystemId, types.LifeCycleStateAvailable)
	s3filesAccessPointState(t, c, ap.AccessPointId, types.LifeCycleStateAvailable)
	got, err := c.GetFileSystem(ctx, &sdk.GetFileSystemInput{FileSystemId: fs.FileSystemId})
	if err != nil || len(got.Tags) != 1 || *got.ClientToken != "persistent" {
		t.Fatalf("file system: %+v %v", got, err)
	}

	pol, err := c.GetFileSystemPolicy(ctx, &sdk.GetFileSystemPolicyInput{FileSystemId: fs.FileSystemId})
	if err != nil || *pol.Policy != policy {
		t.Fatalf("policy: %+v %v", pol, err)
	}

	sync, err := c.GetSynchronizationConfiguration(
		ctx,
		&sdk.GetSynchronizationConfigurationInput{FileSystemId: fs.FileSystemId},
	)
	if err != nil || *sync.LatestVersionNumber != 1 || *sync.ExpirationDataRules[0].DaysAfterLastAccess != 7 {
		t.Fatalf("synchronization: %+v %v", sync, err)
	}

	// The persisted token still replays, and the record can be deleted after its children.
	again, err := c.CreateAccessPoint(
		ctx,
		&sdk.CreateAccessPointInput{FileSystemId: fs.FileSystemId, ClientToken: new("ap")},
	)
	if err != nil || *again.AccessPointId != *ap.AccessPointId {
		t.Fatalf("replay: %+v %v", again, err)
	}

	if _, err = c.DeleteAccessPoint(ctx, &sdk.DeleteAccessPointInput{AccessPointId: ap.AccessPointId}); err != nil {
		t.Fatal(err)
	}

	s3filesAccessPointState(t, c, ap.AccessPointId, "")
	if _, err = c.DeleteFileSystem(ctx, &sdk.DeleteFileSystemInput{FileSystemId: fs.FileSystemId}); err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	s3filesFileSystemState(t, s3filesClient(s, "us-east-1"), fs.FileSystemId, "")
}

func TestS3FilesCloudFormation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := server(t)
	env := s3filesSetup(t, s)
	c := cfn.New(cfn.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	policy := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{"Effect": "Allow", "Principal": "*", "Action": "s3files:ClientMount", "Resource": "*"},
		},
	}

	template := func(team string, days int, groups ...string) *string {
		doc := map[string]any{
			"Resources": map[string]any{
				"Files": map[string]any{"Type": "AWS::S3Files::FileSystem", "Properties": map[string]any{
					"Bucket": "arn:aws:s3:::" + s3filesBucket, "RoleArn": env.role, "Prefix": "stack/",
					"Tags": []map[string]string{{"Key": "team", "Value": team}},
					"SynchronizationConfiguration": map[string]any{
						"ImportDataRules": []map[string]any{
							{"Prefix": "", "Trigger": "ON_FILE_ACCESS", "SizeLessThan": "1024"},
						},
						"ExpirationDataRules": []map[string]any{{"DaysAfterLastAccess": days}},
					},
				}},
				"Policy": map[string]any{"Type": "AWS::S3Files::FileSystemPolicy", "Properties": map[string]any{
					"FileSystemId": map[string]string{"Ref": "Files"}, "Policy": policy,
				}},
				"Mount": map[string]any{"Type": "AWS::S3Files::MountTarget", "Properties": map[string]any{
					"FileSystemId":   map[string]string{"Ref": "Files"},
					"SubnetId":       env.subnetA,
					"SecurityGroups": groups,
				}},
				"Point": map[string]any{"Type": "AWS::S3Files::AccessPoint", "Properties": map[string]any{
					"FileSystemId": map[string]string{"Ref": "Files"},
					"PosixUser":    map[string]any{"Uid": "1000", "Gid": "1000"},
					"RootDirectory": map[string]any{"Path": "/app", "CreationPermissions": map[string]any{
						"OwnerUid": "1000", "OwnerGid": "1000", "Permissions": "755",
					}},
					"Tags": []map[string]string{{"Key": "team", "Value": team}},
				}},
			},
			"Outputs": map[string]any{
				"Arn": map[string]any{"Value": map[string]string{"Ref": "Files"}},
				"Id":  map[string]any{"Value": map[string]any{"Fn::GetAtt": []string{"Files", "FileSystemId"}}},
				"Version": map[string]any{
					"Value": map[string]any{
						"Fn::GetAtt": []string{"Files", "SynchronizationConfiguration.LatestVersionNumber"},
					},
				},
				"Mount":    map[string]any{"Value": map[string]string{"Ref": "Mount"}},
				"Vpc":      map[string]any{"Value": map[string]any{"Fn::GetAtt": []string{"Mount", "VpcId"}}},
				"Point":    map[string]any{"Value": map[string]string{"Ref": "Point"}},
				"PointArn": map[string]any{"Value": map[string]any{"Fn::GetAtt": []string{"Point", "AccessPointArn"}}},
			},
		}

		data, _ := json.Marshal(doc)
		return new(string(data))
	}

	_, err := c.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("files"), TemplateBody: template("core", 30, env.groupA)},
	)
	if err != nil {
		t.Fatal(err)
	}

	stack := settled(ctx, t, c, "files")
	out := outputsOf(&stack)
	if string(stack.StackStatus) != "CREATE_COMPLETE" || out["Arn"] == "" || out["Id"] == "" || out["Version"] != "1" ||
		out["Vpc"] != env.vpc || out["PointArn"] != out["Arn"]+"/access-point/"+out["Point"] {
		t.Fatalf("stack: %+v %v", stack, out)
	}

	fs, err := env.client.GetFileSystem(ctx, &sdk.GetFileSystemInput{FileSystemId: new(out["Id"])})
	if err != nil || fs.Status != types.LifeCycleStateAvailable || *fs.Prefix != "stack/" || len(fs.Tags) != 1 {
		t.Fatalf("file system: %+v %v", fs, err)
	}

	pol, err := env.client.GetFileSystemPolicy(ctx, &sdk.GetFileSystemPolicyInput{FileSystemId: new(out["Id"])})
	if err != nil || pol.Policy == nil {
		t.Fatalf("policy: %+v %v", pol, err)
	}

	// Tags, rules and security groups change in place: the same resources come back.
	_, err = c.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("files"), TemplateBody: template("platform", 60, env.groupB)},
	)
	if err != nil {
		t.Fatal(err)
	}

	stack = settled(ctx, t, c, "files")
	updated := outputsOf(&stack)
	if string(stack.StackStatus) != "UPDATE_COMPLETE" || updated["Id"] != out["Id"] ||
		updated["Mount"] != out["Mount"] ||
		updated["Point"] != out["Point"] ||
		updated["Version"] != "2" {
		t.Fatalf("update: %+v %v", stack, updated)
	}

	mt, err := env.client.GetMountTarget(ctx, &sdk.GetMountTargetInput{MountTargetId: new(updated["Mount"])})
	if err != nil || !slices.Equal(mt.SecurityGroups, []string{env.groupB}) {
		t.Fatalf("mount target: %+v %v", mt, err)
	}

	for _, id := range []*string{new(updated["Id"]), new(updated["Point"])} {
		got, e := env.client.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceId: id})
		if e != nil || len(got.Tags) != 1 || *got.Tags[0].Value != "platform" {
			t.Fatalf("tags of %s: %+v %v", *id, got, e)
		}
	}

	if _, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("files")}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "S3 Files stack deletion", func() bool {
		_, e := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: new("files")})
		return e != nil
	})

	s3filesFileSystemState(t, env.client, new(out["Id"]), "")
	s3filesMountTargetState(t, env.client, new(updated["Mount"]), "")
}
