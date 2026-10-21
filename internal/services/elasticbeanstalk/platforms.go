package elasticbeanstalk

import (
	"context"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/elasticbeanstalk"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

type platform struct{ branch, version, runtime string }

// Fixed snapshot of AWS's AL2023 catalogue, 2026-10-22:
// https://docs.aws.amazon.com/elasticbeanstalk/latest/platforms/platforms-supported.html
var platforms = []platform{
	{".NET 8 running on 64bit Amazon Linux 2023", "3.11.11", ".NET 8"},
	{"Corretto 21 running on 64bit Amazon Linux 2023", "4.13.1", "Corretto 21"},
	{"Docker running on 64bit Amazon Linux 2023", "4.13.11", "Docker"},
	{"Go 1 running on 64bit Amazon Linux 2023", "4.10.3", "Go 1"},
	{"Node.js 22 running on 64bit Amazon Linux 2023", "6.11.11", "Node.js 22"},
	{"PHP 8.4 running on 64bit Amazon Linux 2023", "4.13.12", "PHP 8.4"},
	{"Python 3.13 running on 64bit Amazon Linux 2023", "4.13.11", "Python 3.13"},
	{"Ruby 3.4 running on 64bit Amazon Linux 2023", "4.14.11", "Ruby 3.4"},
}

func (p platform) stack() string {
	return "64bit Amazon Linux 2023 v" + p.version + " running " + p.runtime
}

func (p platform) arn(region string) *string {
	return new("arn:aws:elasticbeanstalk:" + region + "::platform/" + p.branch + "/" + p.version)
}

func resolvePlatform(sc request.Scope, stack, pa *string) (platform, error) {
	if stack != nil && pa != nil {
		return platform{}, invalid("Specify SolutionStackName or PlatformArn, not both.")
	}

	for _, p := range platforms {
		if (stack != nil && *stack == p.stack()) || (pa != nil && *pa == *p.arn(sc.Region)) {
			return p, nil
		}
	}

	return platform{}, invalid("Unknown solution stack or platform ARN.")
}

func (s *Service) ListAvailableSolutionStacks(
	_ context.Context,
	_ *api.ListAvailableSolutionStacksInput,
) (*api.ListAvailableSolutionStacksResultMessage, error) {
	out := &api.ListAvailableSolutionStacksResultMessage{}
	for _, p := range platforms {
		out.SolutionStacks = append(out.SolutionStacks, p.stack())
		out.SolutionStackDetails = append(
			out.SolutionStackDetails,
			api.SolutionStackDescription{SolutionStackName: new(p.stack()), PermittedFileTypes: []string{"zip", "war"}},
		)
	}

	return out, nil
}

func (s *Service) ListPlatformVersions(
	ctx context.Context,
	in *api.ListPlatformVersionsRequest,
) (*api.ListPlatformVersionsResult, error) {
	sc, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	n, err := limit(in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.ListPlatformVersionsResult{}
	for _, p := range platforms {
		ps := api.PlatformSummary{
			PlatformArn:                  p.arn(sc.Region),
			PlatformOwner:                new("AWSElasticBeanstalk"),
			PlatformStatus:               new(ready),
			PlatformVersion:              new(p.version),
			PlatformBranchName:           new(p.branch),
			PlatformLifecycleState:       new("recommended"),
			PlatformBranchLifecycleState: new("supported"),
			OperatingSystemName:          new("Amazon Linux"),
			OperatingSystemVersion:       new("2023"),
			SupportedTierList:            []string{"WebServer/Standard", "Worker/SQS/HTTP"},
		}

		keep := true
		for _, f := range in.Filters {
			var value string
			switch common.Value(f.Type) {
			case "PlatformName":
				value = p.branch
			case "PlatformVersion":
				value = p.version
			case "PlatformStatus":
				value = ready
			case "PlatformOwner":
				value = "AWSElasticBeanstalk"
			case "PlatformBranchName":
				value = p.branch
			case "PlatformLifecycleState":
				value = "recommended"
			default:
				return nil, invalid("Unsupported platform filter.")
			}

			if len(f.Values) == 0 {
				return nil, invalid("Filter values required.")
			}

			switch common.Value(f.Operator) {
			case "=":
				keep = keep && slices.Contains(f.Values, value)
			case "!=":
				keep = keep && !slices.Contains(f.Values, value)
			case "contains":
				found := false
				for _, v := range f.Values {
					found = found || strings.Contains(value, v)
				}

				keep = keep && found
			default:
				return nil, invalid("Unsupported filter operator.")
			}
		}

		if keep {
			out.PlatformSummaryList = append(out.PlatformSummaryList, ps)
		}
	}

	out.PlatformSummaryList, out.NextToken, err = common.Page(
		out.PlatformSummaryList,
		func(p api.PlatformSummary) string { return *p.PlatformArn },
		in.NextToken,
		n,
		invalid("Invalid NextToken."),
	)
	return out, err
}

func (s *Service) DescribePlatformVersion(
	ctx context.Context,
	in *api.DescribePlatformVersionRequest,
) (*api.DescribePlatformVersionResult, error) {
	sc, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	p, err := resolvePlatform(sc, nil, in.PlatformArn)
	if err != nil {
		return nil, err
	}

	return &api.DescribePlatformVersionResult{
		PlatformDescription: &api.PlatformDescription{
			PlatformArn:                  p.arn(sc.Region),
			PlatformName:                 new(p.branch),
			PlatformBranchName:           new(p.branch),
			PlatformVersion:              new(p.version),
			SolutionStackName:            new(p.stack()),
			PlatformOwner:                new("AWSElasticBeanstalk"),
			PlatformStatus:               new(ready),
			OperatingSystemName:          new("Amazon Linux"),
			OperatingSystemVersion:       new("2023"),
			PlatformLifecycleState:       new("recommended"),
			PlatformBranchLifecycleState: new("supported"),
			SupportedTierList:            []string{"WebServer/Standard", "Worker/SQS/HTTP"},
		},
	}, nil
}
