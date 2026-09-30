package scheduler

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/scheduler"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagMap(tags []api.Tag) map[string]string {
	out := map[string]string{}
	for _, t := range tags {
		out[value(t.Key)] = value(t.Value)
	}

	return out
}

func tagError(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return awserror.New("ServiceQuotaExceededException", err.Error())
	}

	return validation("%s", err.Error())
}

func (s *Service) CreateScheduleGroup(
	ctx context.Context, in *api.CreateScheduleGroupInput,
) (*api.CreateScheduleGroupOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name := value(in.Name)
	if !namePattern.MatchString(name) {
		return nil, validation("The group name must be 1 to 64 letters, digits, and -_. characters.")
	}

	tags := tagMap(in.Tags)
	if err = tagging.Check(tags); err != nil {
		return nil, tagError(err)
	}

	now := unix(s.now())
	return &api.CreateScheduleGroupOutput{ScheduleGroupArn: new(groupARN(m.Scope, name))},
		s.store.Update(ctx, func(tx storage.WriteTx) error {
			if _, exists := tx.Get(key(m.Scope, kindGroup, name)); exists || name == defaultGroup {
				return awserror.New("ConflictException", "Schedule group "+name+" already exists.")
			}

			return write(tx, key(m.Scope, kindGroup, name), group{Name: name, Created: now, Modified: now, Tags: tags})
		})
}

func groupOutput(scope request.Scope, g *group) *api.GetScheduleGroupOutput {
	return &api.GetScheduleGroupOutput{
		Arn: new(groupARN(scope, g.Name)), Name: new(g.Name), State: new(groupActive),
		CreationDate: new(g.Created), LastModificationDate: new(g.Modified),
	}
}

func (s *Service) GetScheduleGroup(
	ctx context.Context, in *api.GetScheduleGroupInput,
) (*api.GetScheduleGroupOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var g *group
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		g, err = loadGroup(tx, m.Scope, value(in.Name))
		return err
	})
	if err != nil {
		return nil, err
	}

	return groupOutput(m.Scope, g), nil
}

// DeleteScheduleGroup deletes a group and every schedule in it at once.
func (s *Service) DeleteScheduleGroup(
	ctx context.Context, in *api.DeleteScheduleGroupInput,
) (*api.DeleteScheduleGroupOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name := value(in.Name)
	if name == defaultGroup {
		return nil, validation("The default schedule group cannot be deleted.")
	}

	return &api.DeleteScheduleGroupOutput{}, s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, loadErr := loadGroup(tx, m.Scope, name); loadErr != nil {
			return loadErr
		}

		for _, k := range tx.Keys(key(m.Scope, kindSchedule, name+"/")) {
			tx.Delete(k)
			tx.Delete(indexKey(m.Scope, name, k[strings.LastIndexByte(k, '/')+1:]))
		}

		tx.Delete(key(m.Scope, kindGroup, name))
		return nil
	})
}

func (s *Service) ListScheduleGroups(
	ctx context.Context, in *api.ListScheduleGroupsInput,
) (*api.ListScheduleGroupsOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	limit := int64(defaultPage)
	if in.MaxResults != nil {
		limit = *in.MaxResults
	}

	if limit < 1 || limit > defaultPage {
		return nil, validation("MaxResults must be 1 to 100.")
	}

	var groups []*group
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		def, _ := loadGroup(tx, m.Scope, defaultGroup)
		groups = append(groups, def)
		for _, k := range tx.Keys(key(m.Scope, kindGroup, "")) {
			g, _, readErr := read[group](tx, k)
			if readErr != nil {
				return readErr
			}

			groups = append(groups, g)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	slices.SortFunc(groups, func(a, b *group) int { return strings.Compare(a.Name, b.Name) })
	out := &api.ListScheduleGroupsOutput{ScheduleGroups: []api.ScheduleGroupSummary{}}
	var last string
	for _, g := range groups {
		if in.NextToken != nil && g.Name <= *in.NextToken || !strings.HasPrefix(g.Name, value(in.NamePrefix)) {
			continue
		}

		if int64(len(out.ScheduleGroups)) == limit {
			out.NextToken = new(last)
			break
		}

		o := groupOutput(m.Scope, g)
		out.ScheduleGroups = append(out.ScheduleGroups, api.ScheduleGroupSummary{
			Arn: o.Arn, Name: o.Name, State: o.State, CreationDate: o.CreationDate,
			LastModificationDate: o.LastModificationDate,
		})
		last = g.Name
	}

	return out, nil
}

// taggedGroup returns the group an ARN names; only schedule groups other than
// the default group have tags.
func taggedGroup(scope request.Scope, arn *string) (string, error) {
	name, ok := strings.CutPrefix(value(arn), groupARN(scope, ""))
	switch {
	case !ok || name == "":
		return "", notFound("Resource " + value(arn))
	case name == defaultGroup:
		return "", validation("The default schedule group cannot be tagged.")
	}

	return name, nil
}

func (s *Service) retag(ctx context.Context, arn *string, change func(map[string]string) (map[string]string, error),
) error {
	m, err := identity(ctx)
	if err != nil {
		return err
	}

	name, err := taggedGroup(m.Scope, arn)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		g, loadErr := loadGroup(tx, m.Scope, name)
		if loadErr != nil {
			return loadErr
		}

		if g.Tags, loadErr = change(g.Tags); loadErr != nil {
			return loadErr
		}

		return write(tx, key(m.Scope, kindGroup, name), g)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceOutput, error) {
	return &api.TagResourceOutput{}, s.retag(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string,
		error,
	) {
		merged, err := tagging.Merge(tags, tagMap(in.Tags))
		if err != nil {
			return nil, tagError(err)
		}

		return merged, nil
	})
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	return &api.UntagResourceOutput{}, s.retag(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string,
		error,
	) {
		return tagging.Remove(tags, in.TagKeys), nil
	})
}

func (s *Service) ListTagsForResource(
	ctx context.Context, in *api.ListTagsForResourceInput,
) (*api.ListTagsForResourceOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name, err := taggedGroup(m.Scope, in.ResourceArn)
	if err != nil {
		return nil, err
	}

	var g *group
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		g, err = loadGroup(tx, m.Scope, name)
		return err
	})
	if err != nil {
		return nil, err
	}

	out := &api.ListTagsForResourceOutput{Tags: []api.Tag{}}
	for _, k := range slices.Sorted(maps.Keys(g.Tags)) {
		out.Tags = append(out.Tags, api.Tag{Key: new(k), Value: new(g.Tags[k])})
	}

	return out, nil
}

// Resources lists every schedule group and schedule, for the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, service)
			if !ok {
				continue
			}

			scope := request.Scope{AccountID: account, Region: region}
			switch {
			case strings.HasPrefix(rest, kindGroup+"/"):
				g, _, err := read[group](tx, k)
				if err != nil {
					return err
				}

				out = append(out, catalog.Resource{
					Type: "schedule-group", Account: account, Region: region, Name: g.Name,
					ARN: groupARN(scope, g.Name), Tags: g.Tags,
				})
			case strings.HasPrefix(rest, kindSchedule+"/"):
				rec, _, err := read[record](tx, k)
				if err != nil {
					return err
				}

				sc := &rec.Schedule
				details := map[string]any{
					"Group": value(sc.GroupName), "Expression": value(sc.ScheduleExpression),
					"State": value(sc.State), "Target": value(sc.Target.Arn),
				}
				if rec.Next != 0 {
					details["Next"] = fromUnix(rec.Next).Format("2006-01-02T15:04:05Z")
				}

				out = append(out, catalog.Resource{
					Type: "schedule", Account: account, Region: region, Name: value(sc.Name), ARN: value(sc.Arn),
					Details: details,
				})
			}
		}

		return nil
	})

	return out, err
}
