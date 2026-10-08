package resourcegroups

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/resourcegroups"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/listing"
)

const (
	queryTags  = "TAG_FILTERS_1_0"
	queryStack = "CLOUDFORMATION_STACK_1_0"

	allSupported = "AWS::AllSupported"

	maxQuery        = 4096
	maxTagFilters   = 50
	maxFilterValues = 20
	maxTypeFilters  = 100
)

var (
	typePattern  = regexp.MustCompile(`^AWS::[a-zA-Z0-9]+::[a-zA-Z0-9]+$`)
	stackPattern = regexp.MustCompile(`^arn:aws(?:-[a-z]+)*:cloudformation:[a-z]{2}(?:-[a-z]+)+-\d:\d{12}:stack/.+$`)
)

// tagQuery is the Query of a TAG_FILTERS_1_0 resource query.
type tagQuery struct {
	ResourceTypeFilters []string
	TagFilters          []tagFilter
}

type tagFilter struct {
	Key    string
	Values []string
}

// stackQuery is the Query of a CLOUDFORMATION_STACK_1_0 resource query.
type stackQuery struct {
	StackIdentifier     string
	ResourceTypeFilters []string
}

// decodeQuery reads the JSON document of a resource query into v, rejecting
// members it does not know.
func decodeQuery(text string, v any) error {
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		return bad("The resource query is not valid: %s.", err.Error())
	}

	if dec.More() {
		return bad("The resource query is not valid: unexpected data after the JSON document.")
	}

	return nil
}

func checkTypeFilters(filters []string) error {
	if len(filters) > maxTypeFilters {
		return bad("ResourceTypeFilters can hold at most %d types.", maxTypeFilters)
	}

	for _, f := range filters {
		if f != allSupported && !typePattern.MatchString(f) {
			return bad("%q is not a valid resource type.", f)
		}
	}

	return nil
}

// checkQuery validates a resource query: its type, and the JSON document of
// the query for that type.
func checkQuery(q *api.ResourceQuery) error {
	text := common.Value(q.Query)
	if utf8.RuneCountInString(text) > maxQuery {
		return bad("Query can be at most %d characters long.", maxQuery)
	}

	switch common.Value(q.Type) {
	case queryTags:
		var tq tagQuery
		if err := decodeQuery(text, &tq); err != nil {
			return err
		}

		if err := checkTypeFilters(tq.ResourceTypeFilters); err != nil {
			return err
		}

		if len(tq.TagFilters) > maxTagFilters {
			return bad("TagFilters can hold at most %d filters.", maxTagFilters)
		}

		for _, f := range tq.TagFilters {
			if n := utf8.RuneCountInString(f.Key); n < 1 || n > 128 {
				return bad("Each tag filter needs a key of 1 to 128 characters.")
			}

			if len(f.Values) > maxFilterValues {
				return bad("A tag filter can list at most %d values.", maxFilterValues)
			}
		}
	case queryStack:
		var sq stackQuery
		if err := decodeQuery(text, &sq); err != nil {
			return err
		}

		if !stackPattern.MatchString(sq.StackIdentifier) {
			return bad("StackIdentifier must be the ARN of a CloudFormation stack.")
		}

		return checkTypeFilters(sq.ResourceTypeFilters)
	default:
		return bad("Type must be %s or %s.", queryTags, queryStack)
	}

	return nil
}

// typeMatches reports whether a resource of resourceType passes the filters:
// AWS::AllSupported takes every resource of a service with a namespace, and no
// filter at all takes the same.
func typeMatches(filters []string, resourceType string) bool {
	return len(filters) == 0 || slices.Contains(filters, allSupported) || slices.Contains(filters, resourceType)
}

// unsupported reports the type filters that name a service TinyStack has no
// resources for.
func unsupported(filters []string) []api.QueryError {
	var out []api.QueryError
	for _, f := range filters {
		if f != allSupported && !knownNamespace(f) {
			out = append(out, api.QueryError{
				ErrorCode: new("RESOURCE_TYPE_NOT_SUPPORTED"),
				Message:   new(fmt.Sprintf("Resource type %s is not supported.", f)),
			})
		}
	}

	return out
}

// member is a resource a query found.
type member struct {
	arn          string
	resourceType string
}

// resolve runs a resource query against the resources the caller's account
// and Region hold, keeping the ones whose type passes extra (a request's own
// resource type filters). Query errors are the ones AWS reports in the
// response instead of failing the call.
func (s *Service) resolve(ctx context.Context, q *api.ResourceQuery, extra []string) (
	[]member, []api.QueryError, error,
) {
	resources, err := listing.Scoped(ctx, s.caller)
	if err != nil {
		return nil, nil, err
	}

	var found []member
	var queryErrors []api.QueryError
	switch common.Value(q.Type) {
	case queryTags:
		var tq tagQuery
		if decodeErr := decodeQuery(common.Value(q.Query), &tq); decodeErr != nil {
			return nil, nil, decodeErr
		}

		queryErrors = unsupported(tq.ResourceTypeFilters)
		filters := make([]listing.TagFilter, len(tq.TagFilters))
		for i, f := range tq.TagFilters {
			filters[i] = listing.TagFilter{Key: f.Key, Values: f.Values}
		}

		for i := range resources {
			t, ok := resourceType(&resources[i])
			if ok && typeMatches(tq.ResourceTypeFilters, t) && listing.MatchesTags(&resources[i], filters) {
				found = append(found, member{arn: resources[i].ARN, resourceType: t})
			}
		}
	case queryStack:
		var sq stackQuery
		if decodeErr := decodeQuery(common.Value(q.Query), &sq); decodeErr != nil {
			return nil, nil, decodeErr
		}

		queryErrors = unsupported(sq.ResourceTypeFilters)
		found, queryErrors = s.stackMembers(ctx, &sq, resources, queryErrors)
	}

	queryErrors = append(queryErrors, unsupported(extra)...)
	found = slices.DeleteFunc(found, func(m member) bool { return !typeMatches(extra, m.resourceType) })

	return found, queryErrors, nil
}

// stackMembers lists the inventory resources of a CloudFormation stack: each
// resource the stack records is matched, by its CloudFormation type, to the
// inventory resource whose ARN, name, or last path segment is its physical
// ID. A stack resource TinyStack's inventory does not list is left out.
func (s *Service) stackMembers(
	ctx context.Context, q *stackQuery, resources []catalog.Owned, queryErrors []api.QueryError,
) ([]member, []api.QueryError) {
	out, err := s.caller.Call(ctx, "cloudformation", "ListStackResources",
		map[string]any{"StackName": q.StackIdentifier})
	if err != nil {
		return nil, append(queryErrors, api.QueryError{
			ErrorCode: new("CLOUDFORMATION_STACK_NOT_EXISTING"),
			Message:   new("The stack " + q.StackIdentifier + " does not exist."),
		})
	}

	summaries, _ := out["StackResourceSummaries"].([]any)

	var found []member
	for _, item := range summaries {
		summary, _ := item.(map[string]any)
		stackType, _ := summary["ResourceType"].(string)
		physical, _ := summary["PhysicalResourceId"].(string)

		if physical == "" || !typeMatches(q.ResourceTypeFilters, stackType) {
			continue
		}

		for i := range resources {
			if t, ok := resourceType(&resources[i]); ok && t == stackType && physicalMatches(&resources[i], physical) {
				found = append(found, member{arn: resources[i].ARN, resourceType: t})
			}
		}
	}

	slices.SortFunc(found, func(a, b member) int { return strings.Compare(a.arn, b.arn) })

	return slices.CompactFunc(found, func(a, b member) bool { return a.arn == b.arn }), queryErrors
}

// physicalMatches reports whether a stack resource's physical ID names r: its
// ARN or name, a URL or path ending in its name (an SQS queue URL), or the
// trailing ID of its ARN.
func physicalMatches(r *catalog.Owned, physical string) bool {
	last := physical[strings.LastIndexAny(physical, "/")+1:]

	return physical == r.ARN || physical == r.Name || last == r.Name ||
		strings.HasSuffix(r.ARN, "/"+physical) || strings.HasSuffix(r.ARN, ":"+physical)
}
