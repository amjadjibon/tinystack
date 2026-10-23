package resourceexplorer2

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/resourceexplorer2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/listing"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// term is one keyword or filter of a query. A filter's comma-separated values are alternatives.
type term struct {
	prefix string
	values []string
	negate bool
}

const prefixResourceType = "resourcetype"

// filterPrefixes are the Resource Explorer filters TinyStack evaluates.
var filterPrefixes = []string{
	"accountid", "id", "region", prefixResourceType, "service", "tag", "tag.key", "tag.value",
}

// parse reads a query in the documented subset of the Resource Explorer syntax: keywords, filters with
// comma-separated values, a trailing * wildcard and - negation. Quoted phrases, escapes and other prefixes fail.
func parse(query string) ([]term, error) {
	var out []term
	for _, word := range strings.Fields(query) {
		t := term{}
		if rest, ok := strings.CutPrefix(word, "-"); ok {
			t.negate, word = true, rest
		}

		if word == "" || strings.ContainsAny(word, `"\()|&<>[]{}?`) || word == "OR" || word == "AND" ||
			word == "NOT" {
			return nil, bad()
		}

		prefix, value, isFilter := strings.Cut(word, ":")
		if !isFilter {
			if strings.ContainsAny(word, "=,") || strings.Contains(strings.TrimSuffix(word, "*"), "*") {
				return nil, bad()
			}

			t.values = []string{strings.ToLower(strings.TrimSuffix(word, "*"))}
			out = append(out, t)
			continue
		}

		if !slices.Contains(filterPrefixes, prefix) {
			return nil, bad()
		}

		t.prefix, t.values = prefix, strings.Split(value, ",")
		for _, v := range t.values {
			if !validValue(prefix, v) {
				return nil, bad()
			}
		}

		out = append(out, t)
	}

	return out, nil
}

func validValue(prefix, v string) bool {
	if v == "" || strings.Contains(strings.TrimSuffix(v, "*"), "*") {
		return false
	}

	switch prefix {
	case "tag":
		key, _, ok := strings.Cut(v, "=")
		return v == "all" || v == "none" || ok && key != ""
	case prefixResourceType:
		service, kind, ok := strings.Cut(v, ":")
		return ok && service != "" && kind != ""
	case "id":
		return true
	default:
		return !strings.ContainsAny(v, ":=")
	}
}

// like matches a filter value, whose trailing * matches any suffix.
func like(actual, pattern string, fold bool) bool {
	if fold {
		actual, pattern = strings.ToLower(actual), strings.ToLower(pattern)
	}

	if p, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(actual, p)
	}

	return actual == pattern
}

func region(r *catalog.Owned) string {
	if r.Region == "" {
		return "global"
	}

	return r.Region
}

func matchesFilter(r *catalog.Owned, prefix, v string) bool {
	service, kind := listing.TypeOf(r)
	switch prefix {
	case "accountid":
		return like(r.Account, v, false)
	case "id":
		return like(r.ARN, v, false)
	case "region":
		return like(region(r), v, true)
	case prefixResourceType:
		return like(service+":"+kind, v, true)
	case "service":
		return like(service, v, true)
	case "tag.key":
		return slices.ContainsFunc(slices.Collect(maps.Keys(r.Tags)), func(k string) bool { return like(k, v, false) })
	case "tag.value":
		return slices.ContainsFunc(
			slices.Collect(maps.Values(r.Tags)),
			func(t string) bool { return like(t, v, false) },
		)
	}

	switch v {
	case "all":
		return len(r.Tags) > 0
	case "none":
		return len(r.Tags) == 0
	}

	key, value, _ := strings.Cut(v, "=")
	actual, ok := r.Tags[key]
	return ok && like(actual, value, false)
}

// matches applies AWS's evaluation: keywords are joined with OR, and that group and every filter with AND; a
// negated keyword or filter excludes what it matches.
func matches(r *catalog.Owned, terms []term) bool {
	text := strings.ToLower(r.ARN + " " + r.Name)
	keywords, matched := false, false
	for _, t := range terms {
		var match bool
		if t.prefix == "" {
			match = strings.Contains(text, t.values[0])
		} else {
			match = slices.ContainsFunc(t.values, func(v string) bool { return matchesFilter(r, t.prefix, v) })
		}

		switch {
		case t.negate && match:
			return false
		case t.negate:
		case t.prefix == "":
			keywords, matched = true, matched || match
		case !match:
			return false
		}
	}

	return !keywords || matched
}

func tagFilters(terms []term) bool {
	return slices.ContainsFunc(terms, func(t term) bool { return strings.HasPrefix(t.prefix, "tag") })
}

func (s *Service) Search(ctx context.Context, in *api.SearchInput) (*api.SearchOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	terms, queryErr := parse(common.Value(in.QueryString))
	if queryErr != nil {
		return nil, queryErr
	}

	var view *api.View
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		if _, err := loadIndex(tx, scope); err != nil {
			return err
		}

		a := common.Value(in.ViewArn)
		if a == "" {
			d, ok, err := storage.Read[string](tx, scope.Key(serviceName, "default", "view"))
			if err != nil {
				return err
			}

			if !ok {
				return awserror.New("UnauthorizedException", "No default view is associated with this Region.")
			}

			a = *d
		}

		r, err := loadView(tx, scope, a)
		if err != nil {
			return err
		}

		view = r.View
		return nil
	})
	if err != nil {
		return nil, err
	}

	if view.Filters != nil {
		filters, filterErr := parse(common.Value(view.Filters.FilterString))
		if filterErr != nil {
			return nil, filterErr
		}

		terms = append(terms, filters...)
	}

	includeTags := slices.ContainsFunc(
		view.IncludedProperties,
		func(p api.IncludedProperty) bool { return common.Value(p.Name) == "tags" },
	)
	if tagFilters(terms) && !includeTags {
		return nil, awserror.New("ValidationException", "Tag filters require a view that includes the tags property.")
	}

	resources, err := listing.Scoped(ctx, s.caller)
	if err != nil {
		return nil, err
	}

	reportedAt := new(time.Now().UTC())
	out := &api.SearchOutput{ViewArn: view.ViewArn, Resources: []api.Resource{}}
	for i := range resources {
		r := &resources[i]
		if !matches(r, terms) {
			continue
		}

		service, kind := listing.TypeOf(r)
		found := api.Resource{
			Arn:             new(r.ARN),
			Region:          new(region(r)),
			OwningAccountId: new(r.Account),
			Service:         new(service),
			ResourceType:    new(service + ":" + kind),
			LastReportedAt:  reportedAt,
		}

		if includeTags {
			tags := tagging.List(
				r.Tags,
				func(k, v string) map[string]string { return map[string]string{"Key": k, "Value": v} },
			)
			found.Properties = []api.ResourceProperty{
				{Name: new("tags"), LastReportedAt: reportedAt, Data: new(any(tags))},
			}
		}

		out.Resources = append(out.Resources, found)
	}

	out.Count = &api.ResourceCount{TotalResources: new(int64(len(out.Resources))), Complete: new(true)}
	out.Resources, out.NextToken, err = common.Page(
		out.Resources,
		func(r api.Resource) string { return common.Value(r.Arn) },
		in.NextToken,
		size(in.MaxResults),
		bad(),
	)
	return out, err
}

func (s *Service) ListResources(ctx context.Context, in *api.ListResourcesInput) (*api.ListResourcesOutput, error) {
	query := ""
	if in.Filters != nil {
		query = common.Value(in.Filters.FilterString)
	}

	out, err := s.Search(
		ctx,
		&api.SearchInput{
			QueryString: new(query),
			ViewArn:     in.ViewArn,
			NextToken:   in.NextToken,
			MaxResults:  in.MaxResults,
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.ListResourcesOutput{Resources: out.Resources, NextToken: out.NextToken, ViewArn: out.ViewArn}, nil
}

// Supported types are derived from inventory currently reported for this account
// and Region, rather than claiming support for AWS's unrelated resource types.
func (s *Service) ListSupportedResourceTypes(
	ctx context.Context,
	in *api.ListSupportedResourceTypesInput,
) (*api.ListSupportedResourceTypesOutput, error) {
	resources, err := listing.Scoped(ctx, s.caller)
	if err != nil {
		return nil, err
	}

	types := map[string]string{}
	for i := range resources {
		service, kind := listing.TypeOf(&resources[i])
		if service != "" && kind != "" {
			types[service+":"+kind] = service
		}
	}

	keys := make([]string, 0, len(types))
	for k := range types {
		keys = append(keys, k)
	}

	slices.Sort(keys)
	out := &api.ListSupportedResourceTypesOutput{}
	for _, k := range keys {
		out.ResourceTypes = append(
			out.ResourceTypes,
			api.SupportedResourceType{ResourceType: new(k), Service: new(types[k])},
		)
	}

	out.ResourceTypes, out.NextToken, err = common.Page(
		out.ResourceTypes,
		func(t api.SupportedResourceType) string { return common.Value(t.ResourceType) },
		in.NextToken,
		size(in.MaxResults),
		bad(),
	)
	return out, err
}
