// Package listing is the view of every service's inventory that the Resource
// Groups Tagging API and Resource Groups share: the resources of the caller's
// account and Region, and the matching of their types and tags.
package listing

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

// GlobalRegion is the Region whose callers see global resources (IAM,
// Route 53), which have no Region of their own.
const GlobalRegion = "us-east-1"

// TagFilter selects resources that have Key and, when Values is not empty, one
// of the values.
type TagFilter struct {
	Key    string
	Values []string
}

// Scoped returns the resources in the caller's account and Region, ordered by
// ARN. Global resources belong to GlobalRegion, as AWS returns them.
func Scoped(ctx context.Context, c *catalog.Caller) ([]catalog.Owned, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	all, err := c.Resources(ctx)
	if err != nil {
		return nil, err
	}

	var out []catalog.Owned
	for _, r := range all {
		if r.ARN != "" && r.Account == m.AccountID && cmp.Or(r.Region, GlobalRegion) == m.Region {
			out = append(out, r)
		}
	}

	slices.SortFunc(out, func(a, b catalog.Owned) int { return cmp.Compare(a.ARN, b.ARN) })

	return slices.CompactFunc(out, func(a, b catalog.Owned) bool { return a.ARN == b.ARN }), nil
}

// Tagged returns the resources that have tags.
func Tagged(resources []catalog.Owned) []catalog.Owned {
	return slices.DeleteFunc(slices.Clone(resources), func(r catalog.Owned) bool { return len(r.Tags) == 0 })
}

// TypeOf returns the service and resource type an ARN names, as the Tagging
// API's ResourceTypeFilters write them ("ec2:instance"). An ARN whose
// resource has no type prefix (an SQS queue, an S3 bucket) takes its
// inventory's type.
func TypeOf(r *catalog.Owned) (service, kind string) {
	parts := strings.SplitN(r.ARN, ":", 6)
	if len(parts) < 6 {
		return "", ""
	}

	if i := strings.IndexAny(parts[5], "/:"); i >= 0 {
		return parts[2], parts[5][:i]
	}

	return parts[2], r.Type
}

// MatchesType reports whether r is of one of the types filters name, as
// "service" or "service:type"; no filters match every resource.
func MatchesType(r *catalog.Owned, filters []string) bool {
	if len(filters) == 0 {
		return true
	}

	service, kind := TypeOf(r)

	return slices.ContainsFunc(filters, func(f string) bool { return f == service || f == service+":"+kind })
}

// MatchesTags reports whether r passes every filter: it has the filter's key
// and, when the filter lists values, one of them.
func MatchesTags(r *catalog.Owned, filters []TagFilter) bool {
	for _, f := range filters {
		v, ok := r.Tags[f.Key]
		if !ok || len(f.Values) > 0 && !slices.Contains(f.Values, v) {
			return false
		}
	}

	return true
}

// TagFilters converts a service's own tag filter structures to TagFilter
// values, reading each one's key and values with the given accessors.
func TagFilters[T any](filters []T, key func(T) *string, values func(T) []string) []TagFilter {
	out := make([]TagFilter, len(filters))
	for i, f := range filters {
		out[i] = TagFilter{Key: common.Value(key(f)), Values: values(f)}
	}

	return out
}
