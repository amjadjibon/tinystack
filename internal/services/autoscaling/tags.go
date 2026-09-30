package autoscaling

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/autoscaling"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxTagKey   = 128
	maxTagValue = 256
)

// checkTag checks a tag and returns it as a group reports it.
func checkTag(t api.Tag, group string) (api.TagDescription, error) {
	k, v := value(t.Key), value(t.Value)
	switch {
	case t.ResourceType != nil && *t.ResourceType != resourceTypeGroup:
		return api.TagDescription{}, validation("Tags can only be applied to resources of type auto-scaling-group.")
	case group != "" && t.ResourceId != nil && *t.ResourceId != group:
		return api.TagDescription{}, validation("The tag's ResourceId must be the group's name.")
	case k == "" || len(k) > maxTagKey || len(v) > maxTagValue:
		return api.TagDescription{}, validation("Tag keys must be 1 to 128 characters and values at most 256.")
	case strings.HasPrefix(strings.ToLower(k), "aws:"):
		return api.TagDescription{}, validation("Tag keys starting with 'aws:' are reserved.")
	}

	if group == "" {
		group = value(t.ResourceId)
	}

	return api.TagDescription{
		ResourceId: new(group), ResourceType: new(resourceTypeGroup), Key: new(k), Value: new(v),
		PropagateAtLaunch: new(value(t.PropagateAtLaunch)),
	}, nil
}

// merge sets tags on a group's tags, by key, keeping them sorted.
func merge(tags []api.TagDescription, set ...api.TagDescription) ([]api.TagDescription, error) {
	for _, t := range set {
		tags = slices.DeleteFunc(tags, func(o api.TagDescription) bool { return value(o.Key) == value(t.Key) })
		tags = append(tags, t)
	}

	if len(tags) > maxTags {
		return nil, awserror.New("LimitExceededFault", "The quota of 50 tags per Auto Scaling group has been reached.")
	}

	slices.SortFunc(tags, func(a, b api.TagDescription) int { return strings.Compare(value(a.Key), value(b.Key)) })
	return tags, nil
}

// checkTags checks the tags a new group is created with.
func checkTags(tags []api.Tag, group string) ([]api.TagDescription, error) {
	out := []api.TagDescription{}
	for _, t := range tags {
		d, err := checkTag(t, group)
		if err != nil {
			return nil, err
		}

		if out, err = merge(out, d); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// retag changes the tags of the groups tags name, grouped by their
// ResourceId.
func (s *Service) retag(ctx context.Context, tags []api.Tag,
	change func(scope request.Scope, g *group, t api.TagDescription) error,
) error {
	m, err := identity(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, t := range tags {
			d, checkErr := checkTag(t, "")
			if checkErr != nil {
				return checkErr
			}

			g, loadErr := loadGroup(tx, m.Scope, value(d.ResourceId))
			if loadErr != nil {
				return loadErr
			}

			if changeErr := change(m.Scope, g, d); changeErr != nil {
				return changeErr
			}

			if writeErr := write(tx, key(m.Scope, kindGroup, g.name()), g); writeErr != nil {
				return writeErr
			}
		}

		return nil
	})
}

func (s *Service) CreateOrUpdateTags(
	ctx context.Context, in *api.CreateOrUpdateTagsType,
) (*api.CreateOrUpdateTagsOutput, error) {
	return &api.CreateOrUpdateTagsOutput{}, s.retag(ctx, in.Tags, func(_ request.Scope, g *group,
		t api.TagDescription,
	) error {
		var err error
		g.Group.Tags, err = merge(g.Group.Tags, t)
		return err
	})
}

func (s *Service) DeleteTags(ctx context.Context, in *api.DeleteTagsType) (*api.DeleteTagsOutput, error) {
	return &api.DeleteTagsOutput{}, s.retag(ctx, in.Tags, func(_ request.Scope, g *group,
		t api.TagDescription,
	) error {
		g.Group.Tags = slices.DeleteFunc(g.Group.Tags, func(o api.TagDescription) bool {
			return value(o.Key) == value(t.Key)
		})
		return nil
	})
}

func tagValues(t api.TagDescription) map[string]string {
	return map[string]string{
		"auto-scaling-group": value(t.ResourceId), "key": value(t.Key), "value": value(t.Value),
		"propagate-at-launch": strconv.FormatBool(value(t.PropagateAtLaunch)),
	}
}

func (s *Service) DescribeTags(ctx context.Context, in *api.DescribeTagsType) (*api.TagsType, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	for _, f := range in.Filters {
		if _, ok := tagValues(api.TagDescription{})[value(f.Name)]; !ok {
			return nil, validation("Filter name %s is not valid.", value(f.Name))
		}
	}

	var all []api.TagDescription
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(key(m.Scope, kindGroup, "")) {
			g, _, readErr := read[group](tx, k)
			if readErr != nil {
				return readErr
			}

			for _, t := range g.Group.Tags {
				v := tagValues(t)
				if !slices.ContainsFunc(in.Filters, func(f api.Filter) bool {
					return !slices.Contains(f.Values, v[value(f.Name)])
				}) {
					all = append(all, t)
				}
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	id := func(t api.TagDescription) string { return value(t.ResourceId) + "\x00" + value(t.Key) }
	page, next, err := window(all, id, in.NextToken, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	return &api.TagsType{Tags: page, NextToken: next}, nil
}
