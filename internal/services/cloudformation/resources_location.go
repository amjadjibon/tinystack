package cloudformation

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"regexp"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/location"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/location"
)

const (
	locationCreateTime         = "CreateTime"
	locationUpdateTime         = "UpdateTime"
	locationTrackerType        = "AWS::Location::Tracker"
	locationName               = "TrackerName"
	locationFiltering          = "PositionFiltering"
	locationPrevious           = "LocationPreviousRecord"
	locationPreviousProperties = "LocationPreviousProperties"
	locationCommitted          = "LocationCommittedRevision"
)

var (
	//nolint:gocritic // Preserve the exact documented CF character range; API rules also apply.
	locationCFTagKey = regexp.MustCompile(`^[a-zA-Z+-=._:/]+$`)
	//nolint:gocritic // Preserve the exact documented CF character range; API rules also apply.
	locationCFTagValue = regexp.MustCompile(`^[A-Za-z0-9 _=@:.+-/]*$`)
)

type locationRecords interface {
	CreateTracker(context.Context, *api.CreateTrackerRequest) (*api.CreateTrackerResponse, error)
	DescribeTracker(context.Context, *api.DescribeTrackerRequest) (*api.DescribeTrackerResponse, error)
	DeleteTracker(context.Context, *api.DeleteTrackerRequest) (*api.DeleteTrackerResponse, error)
	CloudFormationUpdate(
		context.Context,
		*api.UpdateTrackerRequest,
		map[string]string,
		map[string]string,
	) (*location.Snapshot, *location.Snapshot, error)
	CloudFormationRestore(context.Context, *location.Snapshot, string) (*location.Snapshot, error)
}

func (s *Service) UseLocation(records locationRecords) { s.location = records }
func (c *client) locationContext(ctx context.Context) context.Context {
	return request.WithMetadata(ctx, request.Metadata{Scope: c.scope, Service: "location", Endpoint: c.endpoint})
}

func locationAttributes(t *api.DescribeTrackerResponse) map[string]any {
	return map[string]any{
		memberArn:          *t.TrackerArn,
		"TrackerArn":       *t.TrackerArn,
		locationCreateTime: t.CreateTime.Format(time.RFC3339Nano),
		locationUpdateTime: t.UpdateTime.Format(time.RFC3339Nano),
	}
}

func locationParams(p props) (*api.CreateTrackerRequest, error) {
	// CF Tags is a Key/Value list, whereas the tracker API uses a map.
	clean := maps.Clone(p)
	delete(clean, memberTags)
	for k, v := range clean {
		if v == nil {
			return nil, validation("Location property %s cannot be null", k)
		}
	}
	raw, e := json.Marshal(clean)
	if e != nil {
		return nil, e
	}
	var in api.CreateTrackerRequest
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e = d.Decode(&in); e != nil {
		return nil, validation("Invalid or unsupported Location property member/type")
	}
	if in.TrackerName == nil {
		return nil, validation("Location TrackerName is required")
	}
	if in.Description == nil {
		in.Description = new("")
	}
	if in.PositionFiltering == nil {
		in.PositionFiltering = new("TimeBased")
	}
	in.Tags = map[string]string{}
	if value, has := p[memberTags]; has {
		list, ok := value.([]any)
		if !ok {
			return nil, validation("Location Tags requires Key/Value objects")
		}
		for _, value := range list {
			tag, ok := value.(map[string]any)
			if !ok || len(tag) != 2 {
				return nil, validation("Location Tag requires exactly Key and Value")
			}
			key, ok := tag[memberKey].(string)
			if !ok {
				return nil, validation("Location Tag Key must be a string")
			}
			val, ok := tag[memberValue].(string)
			if !ok {
				return nil, validation("Location Tag Value must be a string")
			}
			if !locationCFTagKey.MatchString(key) || !locationCFTagValue.MatchString(val) {
				return nil, validation("Location CF tag key/value violates the documented character patterns")
			}
			if _, exists := in.Tags[key]; exists {
				return nil, validation("Duplicate Location tag key")
			}
			in.Tags[key] = val
		}
	}
	e = location.ValidateConfiguration(
		&api.DescribeTrackerResponse{
			TrackerName:       in.TrackerName,
			Description:       in.Description,
			PositionFiltering: in.PositionFiltering,
			Tags:              in.Tags,
		},
	)
	return &in, e
}

func locationPreflight(old, p props) error {
	if _, e := locationParams(p); e != nil {
		return e
	}
	// No documented description removal behavior is claimed. Explicit empty clears.
	if _, was := old[memberDescription]; was {
		if _, has := p[memberDescription]; !has {
			return validation("Removing Location Description is unsupported; use an explicit empty string to clear")
		}
	}
	return nil
}

func init() {
	register("AWS::Location::Tracker", &resourceType{
		properties: []string{locationName, memberDescription, locationFiltering, memberTags},
		replace:    []string{locationName},
		attributes: []string{memberArn, "TrackerArn", locationCreateTime, locationUpdateTime},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			in, e := locationParams(p)
			if e != nil {
				return "", nil, e
			}
			if c.location == nil {
				return "", nil, validation("Location record adapter is unavailable")
			}
			out, e := c.location.CreateTracker(c.locationContext(ctx), in)
			if e != nil {
				return "", nil, e
			}
			t, e := c.location.DescribeTracker(
				c.locationContext(ctx),
				&api.DescribeTrackerRequest{TrackerName: out.TrackerName},
			)
			if e != nil {
				return "", nil, e
			}
			return *out.TrackerName, locationAttributes(t), nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if e := locationPreflight(props(r.Properties), p); e != nil {
				return nil, e
			}
			in, e := locationParams(p)
			if e != nil {
				return nil, e
			}
			old, e := locationParams(props(r.Properties))
			if e != nil {
				return nil, e
			}
			before, after, e := c.location.CloudFormationUpdate(
				c.locationContext(ctx),
				&api.UpdateTrackerRequest{
					TrackerName:       new(r.Physical),
					Description:       in.Description,
					PositionFiltering: in.PositionFiltering,
				},
				old.Tags,
				in.Tags,
			)
			if e != nil {
				return nil, e
			}
			raw, e := json.Marshal(before)
			if e != nil {
				return nil, e
			}
			var b map[string]any
			if decodeErr := json.Unmarshal(raw, &b); decodeErr != nil {
				return nil, decodeErr
			}
			attrs := locationAttributes(after.Tracker)
			attrs[locationPrevious] = b
			attrs[locationPreviousProperties] = maps.Clone(r.Properties)
			attrs[locationCommitted] = after.Revision
			return attrs, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, e := c.location.DeleteTracker(
				c.locationContext(ctx),
				&api.DeleteTrackerRequest{TrackerName: new(r.Physical)},
			)
			if gone(e, "ResourceNotFoundException") {
				return nil
			}
			return e
		},
	})
}

func locationRestoreReplacement(st *stack, next *step) (physical string, attributes map[string]any, found bool) {
	if !st.Op.Rollback || next.typ != locationTrackerType {
		return "", nil, false
	}
	for _, old := range st.Replaced {
		if old.Logical == next.logical && old.Resource.Type == next.typ &&
			reflect.DeepEqual(props(old.Resource.Properties), next.props) {
			return old.Resource.Physical, old.Resource.Attributes, true
		}
	}
	return "", nil, false
}

func locationRead(ctx context.Context, c *client, name string) (map[string]any, error) {
	out, e := c.location.DescribeTracker(c.locationContext(ctx), &api.DescribeTrackerRequest{TrackerName: new(name)})
	if e != nil {
		return nil, e
	}
	return locationAttributes(out), nil
}

func locationRestoreUpdate(
	ctx context.Context,
	c *client,
	r *resource,
	p props,
) (attributes map[string]any, found bool, err error) {
	b, ok := r.Attributes[locationPrevious].(map[string]any)
	previous, has := r.Attributes[locationPreviousProperties].(map[string]any)
	if !ok || !has || !reflect.DeepEqual(props(previous), p) {
		return nil, false, nil
	}
	raw, e := json.Marshal(b)
	if e != nil {
		return nil, true, e
	}
	var snapshot location.Snapshot
	if decodeErr := json.Unmarshal(raw, &snapshot); decodeErr != nil {
		return nil, true, decodeErr
	}
	expected, ok := r.Attributes[locationCommitted].(string)
	if !ok {
		return nil, true, validation("Missing Location compensation revision")
	}
	after, e := c.location.CloudFormationRestore(c.locationContext(ctx), &snapshot, expected)
	if e != nil {
		return nil, true, e
	}
	return locationAttributes(after.Tracker), true, nil
}
