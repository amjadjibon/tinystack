package autoscaling

import (
	"context"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/autoscaling"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every group, launch configuration, and scaling policy, for
// the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, service)
			if !ok {
				continue
			}

			switch {
			case strings.HasPrefix(rest, kindGroup+"/"):
				g, _, err := read[group](tx, k)
				if err != nil {
					return err
				}

				tags := map[string]string{}
				for _, t := range g.Group.Tags {
					tags[value(t.Key)] = value(t.Value)
				}

				out = append(out, catalog.Resource{
					Type: "auto-scaling-group", Account: account, Region: region, Name: g.name(),
					ARN: value(g.Group.AutoScalingGroupARN), Tags: tags, Details: map[string]any{
						"Min": value(g.Group.MinSize), "Max": value(g.Group.MaxSize),
						"Desired": value(g.Group.DesiredCapacity), "Instances": strconv.Itoa(len(g.Members)),
					},
				})
			case strings.HasPrefix(rest, kindConfiguration+"/"):
				lc, _, err := read[api.LaunchConfiguration](tx, k)
				if err != nil {
					return err
				}

				out = append(out, catalog.Resource{
					Type: "launch-configuration", Account: account, Region: region,
					Name: value(lc.LaunchConfigurationName), ARN: value(lc.LaunchConfigurationARN),
				})
			case strings.HasPrefix(rest, kindPolicy+"/"):
				p, _, err := read[api.ScalingPolicy](tx, k)
				if err != nil {
					return err
				}

				out = append(out, catalog.Resource{
					Type: "scaling-policy", Account: account, Region: region, Name: value(p.PolicyName),
					ARN: value(p.PolicyARN), Details: map[string]any{
						"Group": value(p.AutoScalingGroupName), "Type": value(p.PolicyType),
					},
				})
			}
		}

		return nil
	})

	return out, err
}
