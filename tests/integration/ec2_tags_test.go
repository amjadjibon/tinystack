package integration_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func ec2Filter(name string, values ...string) ec2types.Filter {
	return ec2types.Filter{Name: aws.String(name), Values: values}
}

func ec2Tags(pairs ...string) []ec2types.Tag {
	var tags []ec2types.Tag
	for i := 0; i+1 < len(pairs); i += 2 {
		tags = append(tags, ec2types.Tag{Key: aws.String(pairs[i]), Value: aws.String(pairs[i+1])})
	}

	return tags
}

func TestEC2FiltersAndTags(t *testing.T) {
	s := server(t)
	c := ec2Client(s, "test", "us-east-1")
	ctx := context.Background()
	web, err := c.CreateVpc(ctx, &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeVpc, Tags: ec2Tags("Name", "web-prod", "team", "core"),
		}},
	})
	if err != nil || len(web.Vpc.Tags) != 2 {
		t.Fatalf("got %+v, %v", web, err)
	}

	webID := aws.ToString(web.Vpc.VpcId)
	_, err = c.CreateVpc(ctx, &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.1.0.0/16"),
		TagSpecifications: []ec2types.TagSpecification{
			{ResourceType: ec2types.ResourceTypeSubnet, Tags: ec2Tags("a", "b")},
		},
	})
	code(t, err, "InvalidParameterValue")

	vpcIDs := func(filters ...ec2types.Filter) []string {
		t.Helper()
		out, describeErr := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: filters})
		if describeErr != nil {
			t.Fatal(describeErr)
		}

		var ids []string
		for _, v := range out.Vpcs {
			ids = append(ids, aws.ToString(v.VpcId))
		}

		return ids
	}

	// Values of one filter are ORed and filters ANDed; * and ? are wildcards.
	for name, filters := range map[string][]ec2types.Filter{
		"tag":        {ec2Filter("tag:Name", "web-*")},
		"tag values": {ec2Filter("tag:Name", "nothing", "web-pro?")},
		"tag key":    {ec2Filter("tag-key", "team")},
		"and":        {ec2Filter("is-default", "false"), ec2Filter("cidr", "10.0.0.0/16")},
	} {
		if ids := vpcIDs(filters...); len(ids) != 1 || ids[0] != webID {
			t.Fatalf("%s: %v", name, ids)
		}
	}

	if ids := vpcIDs(ec2Filter("tag:Name", "web-*"), ec2Filter("is-default", "true")); len(ids) != 0 {
		t.Fatalf("got %v", ids)
	}

	_, err = c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: []ec2types.Filter{ec2Filter("color", "red")}})
	code(t, err, "InvalidParameterValue")

	subnets, err := c.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: []ec2types.Filter{
		ec2Filter("default-for-az", "true"), ec2Filter("availability-zone", "us-east-1b", "us-east-1c"),
	}})
	if err != nil || len(subnets.Subnets) != 2 {
		t.Fatalf("got %+v, %v", subnets, err)
	}

	zones, err := c.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{
		Filters: []ec2types.Filter{ec2Filter("zone-name", "*b")},
	})
	if err != nil || len(zones.AvailabilityZones) != 1 {
		t.Fatalf("got %+v, %v", zones, err)
	}

	regions, err := c.DescribeRegions(ctx, &ec2.DescribeRegionsInput{
		Filters: []ec2types.Filter{ec2Filter("region-name", "eu-west-*")},
	})
	if err != nil || len(regions.Regions) != 3 {
		t.Fatalf("got %+v, %v", regions, err)
	}

	// CreateTags changes every resource or none.
	subnetID := subnets.Subnets[0].SubnetId
	_, err = c.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{webID, "subnet-00000000000000000"}, Tags: ec2Tags("env", "dev"),
	})
	code(t, err, "InvalidSubnetID.NotFound")
	if ids := vpcIDs(ec2Filter("tag:env", "dev")); len(ids) != 0 {
		t.Fatalf("partial tagging: %v", ids)
	}

	for errCode, in := range map[string]*ec2.CreateTagsInput{
		"InvalidID":             {Resources: []string{"bucket-1"}, Tags: ec2Tags("a", "b")},
		"InvalidParameterValue": {Resources: []string{webID}, Tags: ec2Tags("aws:owner", "x")},
	} {
		_, err = c.CreateTags(ctx, in)
		code(t, err, errCode)
	}

	var many []string
	for i := range 49 {
		many = append(many, fmt.Sprintf("k%d", i), "v")
	}

	_, err = c.CreateTags(ctx, &ec2.CreateTagsInput{Resources: []string{webID}, Tags: ec2Tags(many...)})
	code(t, err, "TagLimitExceeded")
	_, err = c.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{webID, aws.ToString(subnetID)}, Tags: ec2Tags("env", "dev", "team", "platform"),
	})
	if err != nil {
		t.Fatal(err)
	}

	described, err := c.DescribeTags(ctx, &ec2.DescribeTagsInput{Filters: []ec2types.Filter{
		ec2Filter("resource-id", webID),
	}})
	if err != nil || len(described.Tags) != 3 || aws.ToString(described.Tags[0].Key) != "Name" ||
		described.Tags[0].ResourceType != ec2types.ResourceTypeVpc {
		t.Fatalf("got %+v, %v", described, err)
	}

	byKey, err := c.DescribeTags(ctx, &ec2.DescribeTagsInput{Filters: []ec2types.Filter{
		ec2Filter("key", "env"), ec2Filter("resource-type", "subnet"),
	}})
	if err != nil || len(byKey.Tags) != 1 || aws.ToString(byKey.Tags[0].ResourceId) != aws.ToString(subnetID) {
		t.Fatalf("got %+v, %v", byKey, err)
	}

	var pages, total int
	paginator := ec2.NewDescribeTagsPaginator(c, &ec2.DescribeTagsInput{MaxResults: aws.Int32(5)})
	for paginator.HasMorePages() {
		out, pageErr := paginator.NextPage(ctx)
		if pageErr != nil {
			t.Fatal(pageErr)
		}

		pages, total = pages+1, total+len(out.Tags)
	}

	if pages != 1 || total != 5 {
		t.Fatalf("%d pages, %d tags", pages, total)
	}

	// DeleteTags removes a tag with a value only while it has that value.
	_, err = c.DeleteTags(ctx, &ec2.DeleteTagsInput{
		Resources: []string{webID},
		Tags:      []ec2types.Tag{{Key: aws.String("env"), Value: aws.String("prod")}, {Key: aws.String("team")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if ids := vpcIDs(ec2Filter("tag:env", "dev")); len(ids) != 1 {
		t.Fatalf("env tag removed: %v", ids)
	}

	if ids := vpcIDs(ec2Filter("tag-key", "team")); len(ids) != 0 {
		t.Fatalf("team tag kept: %v", ids)
	}

	if _, err = c.DeleteTags(ctx, &ec2.DeleteTagsInput{Resources: []string{webID}}); err != nil {
		t.Fatal(err)
	}

	if ids := vpcIDs(ec2Filter("tag-key", "*")); len(ids) != 0 {
		t.Fatalf("tags kept: %v", ids)
	}
}
