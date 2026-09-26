package ec2

import (
	"context"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
)

// regions are the commercial Regions TinyStack reports, all enabled.
var regions = []string{
	"af-south-1", "ap-east-1", "ap-northeast-1", "ap-northeast-2", "ap-northeast-3", "ap-south-1", "ap-south-2",
	"ap-southeast-1", "ap-southeast-2", "ap-southeast-3", "ap-southeast-4", "ca-central-1", "eu-central-1",
	"eu-central-2", "eu-north-1", "eu-south-1", "eu-south-2", "eu-west-1", "eu-west-2", "eu-west-3",
	"il-central-1", "me-central-1", "me-south-1", "sa-east-1", "us-east-1", "us-east-2", "us-west-1", "us-west-2",
}

// DescribeRegions lists every Region, or those named. AllRegions changes
// nothing, since no Region needs opting in.
func (s *Service) DescribeRegions(
	ctx context.Context,
	in *api.DescribeRegionsRequest,
) (*api.DescribeRegionsResult, error) {
	if _, err := caller(ctx, in.DryRun); err != nil {
		return nil, err
	}

	for _, name := range in.RegionNames {
		if !slices.Contains(regions, name) {
			return nil, invalidParameter("Invalid region: " + name)
		}
	}

	out := &api.DescribeRegionsResult{Regions: []api.Region{}}
	for _, name := range regions {
		if len(in.RegionNames) == 0 || slices.Contains(in.RegionNames, name) {
			out.Regions = append(out.Regions, api.Region{
				RegionName:  new(name),
				Endpoint:    new("ec2." + name + ".amazonaws.com"),
				OptInStatus: new("opt-in-not-required"),
			})
		}
	}

	return out, nil
}
