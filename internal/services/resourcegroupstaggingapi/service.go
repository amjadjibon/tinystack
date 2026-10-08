// Package resourcegroupstaggingapi implements the Resource Groups Tagging API
// without HTTP dependencies. It keeps no records of its own: it reads the
// resources and tags the other services list in their inventories, and tags
// and untags them through each service's own tag operations
// (catalog.Caller).
package resourcegroupstaggingapi

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/resourcegroupstaggingapi"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/listing"
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

// tagged returns the scope's resources that have tags, which are the ones
// GetResources, GetTagKeys, and GetTagValues report.
func (s *Service) tagged(ctx context.Context) ([]catalog.Owned, error) {
	resources, err := listing.Scoped(ctx, s.caller)

	return listing.Tagged(resources), err
}

// tagFilters converts GetResources' TagFilters.
func tagFilters(filters []api.TagFilter) []listing.TagFilter {
	return listing.TagFilters(filters, func(f api.TagFilter) *string { return f.Key },
		func(f api.TagFilter) []string { return f.Values })
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
		resources, err = listing.Scoped(ctx, s.caller)
		resources = slices.DeleteFunc(resources, func(r catalog.Owned) bool {
			return !slices.Contains(in.ResourceARNList, r.ARN)
		})
	} else {
		filters := tagFilters(in.TagFilters)
		resources, err = s.tagged(ctx)
		resources = slices.DeleteFunc(resources, func(r catalog.Owned) bool {
			return !listing.MatchesType(&r, in.ResourceTypeFilters) || !listing.MatchesTags(&r, filters)
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
