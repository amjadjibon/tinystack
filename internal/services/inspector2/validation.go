package inspector2

import (
	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/inspector2"
)

// validateCriteria checks ordering rules not expressed by Smithy member constraints.
func validateCriteria(c *api.FilterCriteria) error {
	if c == nil {
		return nil
	}

	for _, filters := range [][]api.NumberFilter{c.InspectorScore, c.EcrImageInUseCount, c.EpssScore} {
		for _, f := range filters {
			if f.LowerInclusive != nil && f.UpperInclusive != nil && *f.LowerInclusive > *f.UpperInclusive {
				return awserror.Validation(invalidBounds)
			}
		}
	}

	for _, filters := range [][]api.DateFilter{
		c.FirstObservedAt, c.LastObservedAt, c.UpdatedAt, c.EcrImagePushedAt,
		c.EcrImageLastInUseAt, c.LambdaFunctionLastModifiedAt,
	} {
		for _, f := range filters {
			if f.StartInclusive != nil && f.EndInclusive != nil && *f.StartInclusive > *f.EndInclusive {
				return awserror.Validation(invalidBounds)
			}
		}
	}

	for _, filters := range [][]api.PortRangeFilter{c.PortRange} {
		for _, f := range filters {
			if f.BeginInclusive != nil && f.EndInclusive != nil && *f.BeginInclusive > *f.EndInclusive {
				return awserror.Validation(invalidBounds)
			}
		}
	}

	for _, p := range c.VulnerablePackages {
		if p.Epoch != nil && p.Epoch.LowerInclusive != nil && p.Epoch.UpperInclusive != nil &&
			*p.Epoch.LowerInclusive > *p.Epoch.UpperInclusive {
			return awserror.Validation("Package epoch lower bound exceeds upper bound.")
		}
	}

	return nil
}

func validateCoverage(c *api.CoverageFilterCriteria) error {
	if c == nil {
		return nil
	}

	for _, filters := range [][]api.CoverageNumberFilter{c.EcrImageInUseCount} {
		for _, f := range filters {
			if f.LowerInclusive != nil && f.UpperInclusive != nil && *f.LowerInclusive > *f.UpperInclusive {
				return awserror.Validation("Coverage lower bound exceeds upper bound.")
			}
		}
	}

	for _, filters := range [][]api.CoverageDateFilter{c.LastScannedAt, c.ImagePulledAt, c.EcrImageLastInUseAt} {
		for _, f := range filters {
			if f.StartInclusive != nil && f.EndInclusive != nil && *f.StartInclusive > *f.EndInclusive {
				return awserror.Validation("Coverage lower bound exceeds upper bound.")
			}
		}
	}

	return nil
}
