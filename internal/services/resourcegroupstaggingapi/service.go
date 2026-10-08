// Package resourcegroupstaggingapi implements the Resource Groups Tagging API
// without HTTP dependencies. It keeps no records of its own: it reads the
// resources and tags the other services list in their inventories, and tags
// and untags them through each service's own tag operations
// (catalog.Caller). EC2 lists no inventory, so its resources are not found.
package resourcegroupstaggingapi

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/resourcegroupstaggingapi"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

const (
	// pageSize is the most resources, keys, or values one page holds, and
	// GetResources' default page.
	pageSize = 100
	// maxListedARNs is the most ARNs GetResources' ResourceARNList may name.
	maxListedARNs = 100
	// maxTagFilters and maxFilterValues bound GetResources' TagFilters.
	maxTagFilters   = 50
	maxFilterValues = 20
	// globalRegion is the Region whose callers see global resources (IAM,
	// Route 53), which have no Region of their own.
	globalRegion = "us-east-1"
)

var _ api.API = (*Service)(nil)

type Service struct {
	// caller lists the other services' resources and runs their tag
	// operations.
	caller *catalog.Caller
}

func New() *Service { return &Service{} }

// UseCaller hands over the caller that reaches every other service. start.go
// calls it before the gateway serves requests.
func (s *Service) UseCaller(c *catalog.Caller) { s.caller = c }

func invalid(format string, args ...any) error {
	return awserror.New("InvalidParameterException", fmt.Sprintf(format, args...))
}

func invalidToken() error { return invalid("The pagination token is invalid.") }

// resources returns the resources in the caller's account and Region, ordered
// by ARN. Global resources belong to globalRegion, as AWS returns them.
func (s *Service) resources(ctx context.Context) ([]catalog.Owned, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	all, err := s.caller.Resources(ctx)
	if err != nil {
		return nil, err
	}

	var out []catalog.Owned
	for _, r := range all {
		if r.ARN != "" && r.Account == m.AccountID && cmp.Or(r.Region, globalRegion) == m.Region {
			out = append(out, r)
		}
	}

	slices.SortFunc(out, func(a, b catalog.Owned) int { return cmp.Compare(a.ARN, b.ARN) })

	return slices.CompactFunc(out, func(a, b catalog.Owned) bool { return a.ARN == b.ARN }), nil
}

// tagged returns the scope's resources that have tags, which are the ones
// GetResources, GetTagKeys, and GetTagValues report.
func (s *Service) tagged(ctx context.Context) ([]catalog.Owned, error) {
	resources, err := s.resources(ctx)

	return slices.DeleteFunc(resources, func(r catalog.Owned) bool { return len(r.Tags) == 0 }), err
}

// resourceType returns the service and resource type an ARN names, as
// ResourceTypeFilters write them ("ec2:instance"). An ARN whose resource has
// no type prefix (an SQS queue, an S3 bucket) takes its inventory's type.
func resourceType(r *catalog.Owned) (service, kind string) {
	parts := strings.SplitN(r.ARN, ":", 6)
	if len(parts) < 6 {
		return "", ""
	}

	if i := strings.IndexAny(parts[5], "/:"); i >= 0 {
		return parts[2], parts[5][:i]
	}

	return parts[2], r.Type
}

// matchesType reports whether r is of one of the types filters name, as
// "service" or "service:type"; no filters match every resource.
func matchesType(r *catalog.Owned, filters []string) bool {
	if len(filters) == 0 {
		return true
	}

	service, kind := resourceType(r)

	return slices.ContainsFunc(filters, func(f string) bool { return f == service || f == service+":"+kind })
}

// matchesTags reports whether r passes every filter: it has the filter's key
// and, when the filter lists values, one of them.
func matchesTags(r *catalog.Owned, filters []api.TagFilter) bool {
	for _, f := range filters {
		v, ok := r.Tags[common.Value(f.Key)]
		if !ok || len(f.Values) > 0 && !slices.Contains(f.Values, v) {
			return false
		}
	}

	return true
}

func checkGetResources(in *api.GetResourcesInput) error {
	if n := common.Value(in.ResourcesPerPage); in.ResourcesPerPage != nil && (n < 1 || n > pageSize) {
		return invalid("ResourcesPerPage must be between 1 and %d.", pageSize)
	}

	if len(in.ResourceARNList) > 0 && (len(in.TagFilters) > 0 || len(in.ResourceTypeFilters) > 0) {
		return invalid("ResourceARNList cannot be combined with TagFilters or ResourceTypeFilters.")
	}

	if len(in.ResourceARNList) > maxListedARNs {
		return invalid("ResourceARNList can name at most %d resources.", maxListedARNs)
	}

	if len(in.TagFilters) > maxTagFilters {
		return invalid("TagFilters can hold at most %d filters.", maxTagFilters)
	}

	for _, f := range in.TagFilters {
		if n := utf8.RuneCountInString(common.Value(f.Key)); n < 1 || n > 128 {
			return invalid("Each tag filter needs a key of 1 to 128 characters.")
		}

		if len(f.Values) > maxFilterValues {
			return invalid("A tag filter can list at most %d values.", maxFilterValues)
		}
	}

	return nil
}

// GetResources lists the scope's tagged resources that pass the filters, or
// the resources ResourceARNList names, tagged or not.
func (s *Service) GetResources(ctx context.Context, in *api.GetResourcesInput) (*api.GetResourcesOutput, error) {
	if err := checkGetResources(in); err != nil {
		return nil, err
	}

	var resources []catalog.Owned
	var err error
	if len(in.ResourceARNList) > 0 {
		resources, err = s.resources(ctx)
		resources = slices.DeleteFunc(resources, func(r catalog.Owned) bool {
			return !slices.Contains(in.ResourceARNList, r.ARN)
		})
	} else {
		resources, err = s.tagged(ctx)
		resources = slices.DeleteFunc(resources, func(r catalog.Owned) bool {
			return !matchesType(&r, in.ResourceTypeFilters) || !matchesTags(&r, in.TagFilters)
		})
	}

	if err != nil {
		return nil, err
	}

	limit := pageSize
	if in.ResourcesPerPage != nil {
		limit = int(*in.ResourcesPerPage)
	}

	page, next, err := common.Page(resources, func(r catalog.Owned) string { return r.ARN },
		in.PaginationToken, limit, invalidToken())
	if err != nil {
		return nil, err
	}

	out := &api.GetResourcesOutput{PaginationToken: next}
	for _, r := range page {
		out.ResourceTagMappingList = append(out.ResourceTagMappingList, api.ResourceTagMapping{
			ResourceARN: new(r.ARN),
			Tags:        tagging.List(r.Tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} }),
		})
	}

	return out, nil
}

// GetTagKeys lists the tag keys of the scope's resources.
func (s *Service) GetTagKeys(ctx context.Context, in *api.GetTagKeysInput) (*api.GetTagKeysOutput, error) {
	resources, err := s.tagged(ctx)
	if err != nil {
		return nil, err
	}

	keys := map[string]bool{}
	for _, r := range resources {
		for k := range r.Tags {
			keys[k] = true
		}
	}

	page, next, err := common.Page(slices.Sorted(maps.Keys(keys)), func(k string) string { return k },
		in.PaginationToken, pageSize, invalidToken())
	if err != nil {
		return nil, err
	}

	return &api.GetTagKeysOutput{PaginationToken: next, TagKeys: page}, nil
}

// GetTagValues lists the values the scope's resources give one tag key.
func (s *Service) GetTagValues(ctx context.Context, in *api.GetTagValuesInput) (*api.GetTagValuesOutput, error) {
	key := common.Value(in.Key)
	if n := utf8.RuneCountInString(key); n < 1 || n > 128 {
		return nil, invalid("Key must be 1 to 128 characters long.")
	}

	resources, err := s.tagged(ctx)
	if err != nil {
		return nil, err
	}

	values := map[string]bool{}
	for _, r := range resources {
		if v, ok := r.Tags[key]; ok {
			values[v] = true
		}
	}

	page, next, err := common.Page(slices.Sorted(maps.Keys(values)), func(v string) string { return v },
		in.PaginationToken, pageSize, invalidToken())
	if err != nil {
		return nil, err
	}

	return &api.GetTagValuesOutput{PaginationToken: next, TagValues: page}, nil
}
