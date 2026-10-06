package athena

import (
	"context"
	"maps"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/athena"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// workGroup is a workgroup's record.
type workGroup struct {
	Tags      map[string]string
	WorkGroup api.WorkGroup
}

// defaultConfiguration fills in a workgroup configuration's defaults.
func defaultConfiguration(c *api.WorkGroupConfiguration) *api.WorkGroupConfiguration {
	out := common.OrElse(common.Clone(c), &api.WorkGroupConfiguration{})
	out.EnforceWorkGroupConfiguration = common.OrElse(out.EnforceWorkGroupConfiguration, new(true))
	out.PublishCloudWatchMetricsEnabled = common.OrElse(out.PublishCloudWatchMetricsEnabled, new(true))
	out.RequesterPaysEnabled = common.OrElse(out.RequesterPaysEnabled, new(false))
	version := common.OrElse(out.EngineVersion, &api.EngineVersion{})
	out.EngineVersion = &api.EngineVersion{
		SelectedEngineVersion:  common.OrElse(version.SelectedEngineVersion, new("AUTO")),
		EffectiveEngineVersion: new(engineVersion),
	}

	return out
}

// loadWorkGroup reads a workgroup; the primary workgroup exists before any
// change stores it.
func loadWorkGroup(tx storage.ReadTx, scope request.Scope, name string) (*workGroup, error) {
	w, ok, err := storage.Read[workGroup](tx, scope.Key(service, kindWorkGroup, name))
	switch {
	case err != nil:
		return nil, err
	case ok:
		return w, nil
	case name == primary:
		c := defaultConfiguration(nil)
		c.EnforceWorkGroupConfiguration = new(false)
		return &workGroup{WorkGroup: api.WorkGroup{
			Name: new(primary), State: new("ENABLED"), Configuration: c, CreationTime: new(0.0),
		}}, nil
	}

	return nil, invalid("WorkGroup %s is not found.", name)
}

func (s *Service) CreateWorkGroup(
	ctx context.Context,
	in *api.CreateWorkGroupInput,
) (*api.CreateWorkGroupOutput, error) {
	if !namePattern.MatchString(common.Value(in.Name)) {
		return nil, invalid("The workgroup name must be 1 to 128 letters, digits, periods, underscores, or dashes.")
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		key := scope.Key(service, kindWorkGroup, *in.Name)
		if _, found, readErr := storage.Read[workGroup](tx, key); readErr != nil || found || *in.Name == primary {
			return common.ErrorsOr(readErr, invalid("WorkGroup is already created"))
		}

		return storage.Write(tx, key, &workGroup{Tags: tags, WorkGroup: api.WorkGroup{
			Name: in.Name, State: new("ENABLED"), Configuration: defaultConfiguration(in.Configuration),
			Description: in.Description, CreationTime: new(common.Unix(s.now())),
		}})
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateWorkGroupOutput{}, nil
}

func (s *Service) GetWorkGroup(ctx context.Context, in *api.GetWorkGroupInput) (*api.GetWorkGroupOutput, error) {
	out := &api.GetWorkGroupOutput{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		w, err := loadWorkGroup(tx, scope, common.Value(in.WorkGroup))
		if err == nil {
			out.WorkGroup = &w.WorkGroup
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ListWorkGroups(ctx context.Context, in *api.ListWorkGroupsInput) (*api.ListWorkGroupsOutput, error) {
	var all []*workGroup
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		if all, err = storage.Records[workGroup](tx, scope.Prefix(service, kindWorkGroup)); err != nil {
			return err
		}

		if !slices.ContainsFunc(all, func(w *workGroup) bool { return *w.WorkGroup.Name == primary }) {
			p, _ := loadWorkGroup(tx, scope, primary)
			all = append(all, p)
		}

		slices.SortFunc(all, func(a, b *workGroup) int { return strings.Compare(*a.WorkGroup.Name, *b.WorkGroup.Name) })
		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(all, func(w *workGroup) string { return *w.WorkGroup.Name }, in.NextToken,
		in.MaxResults, 50, 50)
	if err != nil {
		return nil, err
	}

	out := &api.ListWorkGroupsOutput{WorkGroups: []api.WorkGroupSummary{}, NextToken: next}
	for _, w := range selected {
		out.WorkGroups = append(out.WorkGroups, api.WorkGroupSummary{
			Name: w.WorkGroup.Name, State: w.WorkGroup.State, Description: w.WorkGroup.Description,
			CreationTime: w.WorkGroup.CreationTime, EngineVersion: w.WorkGroup.Configuration.EngineVersion,
		})
	}

	return out, nil
}

// apply changes a workgroup's configuration as updates say.
func apply(c *api.WorkGroupConfiguration, u *api.WorkGroupConfigurationUpdates) {
	c.EnforceWorkGroupConfiguration = common.OrElse(u.EnforceWorkGroupConfiguration, c.EnforceWorkGroupConfiguration)
	c.PublishCloudWatchMetricsEnabled = common.OrElse(u.PublishCloudWatchMetricsEnabled,
		c.PublishCloudWatchMetricsEnabled)
	c.RequesterPaysEnabled = common.OrElse(u.RequesterPaysEnabled, c.RequesterPaysEnabled)
	c.BytesScannedCutoffPerQuery = common.OrElse(u.BytesScannedCutoffPerQuery, c.BytesScannedCutoffPerQuery)
	if common.Value(u.RemoveBytesScannedCutoffPerQuery) {
		c.BytesScannedCutoffPerQuery = nil
	}

	if u.EngineVersion != nil {
		c.EngineVersion = defaultConfiguration(
			&api.WorkGroupConfiguration{EngineVersion: u.EngineVersion},
		).EngineVersion
	}

	r := u.ResultConfigurationUpdates
	if r == nil {
		return
	}

	rc := common.OrElse(c.ResultConfiguration, &api.ResultConfiguration{})
	rc.OutputLocation = common.OrElse(r.OutputLocation, rc.OutputLocation)
	rc.EncryptionConfiguration = common.OrElse(r.EncryptionConfiguration, rc.EncryptionConfiguration)
	rc.ExpectedBucketOwner = common.OrElse(r.ExpectedBucketOwner, rc.ExpectedBucketOwner)
	if common.Value(r.RemoveOutputLocation) {
		rc.OutputLocation = nil
	}

	if common.Value(r.RemoveEncryptionConfiguration) {
		rc.EncryptionConfiguration = nil
	}

	if common.Value(r.RemoveExpectedBucketOwner) {
		rc.ExpectedBucketOwner = nil
	}

	c.ResultConfiguration = rc
}

func (s *Service) UpdateWorkGroup(
	ctx context.Context,
	in *api.UpdateWorkGroupInput,
) (*api.UpdateWorkGroupOutput, error) {
	if state := common.Value(in.State); state != "" && state != "ENABLED" && state != "DISABLED" {
		return nil, invalid("State must be ENABLED or DISABLED.")
	}

	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		w, err := loadWorkGroup(tx, scope, common.Value(in.WorkGroup))
		if err != nil {
			return err
		}

		w.WorkGroup.Description = common.OrElse(in.Description, w.WorkGroup.Description)
		w.WorkGroup.State = common.OrElse(in.State, w.WorkGroup.State)
		if in.ConfigurationUpdates != nil {
			apply(w.WorkGroup.Configuration, in.ConfigurationUpdates)
		}

		return storage.Write(tx, scope.Key(service, kindWorkGroup, *in.WorkGroup), w)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateWorkGroupOutput{}, nil
}

func (s *Service) DeleteWorkGroup(
	ctx context.Context,
	in *api.DeleteWorkGroupInput,
) (*api.DeleteWorkGroupOutput, error) {
	if common.Value(in.WorkGroup) == primary {
		return nil, invalid("The primary workgroup cannot be deleted.")
	}

	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := loadWorkGroup(tx, scope, common.Value(in.WorkGroup)); err != nil {
			return err
		}

		tx.Delete(scope.Key(service, kindWorkGroup, *in.WorkGroup))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteWorkGroupOutput{}, nil
}

// workGroupOf reads the workgroup an ARN names, the only resource TinyStack
// tags in Athena.
func workGroupOf(tx storage.ReadTx, scope request.Scope, arn *string) (*workGroup, error) {
	prefix := "arn:aws:athena:" + scope.Region + ":" + scope.AccountID + ":workgroup/"
	name, ok := strings.CutPrefix(common.Value(arn), prefix)
	if !ok {
		return nil, invalid("TinyStack tags Athena workgroups only, not %s.", common.Value(arn))
	}

	return loadWorkGroup(tx, scope, name)
}

func (s *Service) retag(ctx context.Context, arn *string, fn func(map[string]string) map[string]string) error {
	return s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		w, err := workGroupOf(tx, scope, arn)
		if err != nil {
			return err
		}

		w.Tags = fn(common.CloneTags(w.Tags))
		if checkErr := tagging.CheckSet(w.Tags); checkErr != nil {
			return invalid("%s", checkErr.Error())
		}

		return storage.Write(tx, scope.Key(service, kindWorkGroup, *w.WorkGroup.Name), w)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceOutput, error) {
	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.retag(ctx, in.ResourceARN, func(tags map[string]string) map[string]string {
		if tags == nil {
			tags = map[string]string{}
		}

		maps.Copy(tags, add)
		return tags
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceOutput{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	err := s.retag(ctx, in.ResourceARN, func(tags map[string]string) map[string]string {
		return tagging.Remove(tags, in.TagKeys)
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceOutput{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceInput,
) (*api.ListTagsForResourceOutput, error) {
	var tags map[string]string
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		w, err := workGroupOf(tx, scope, in.ResourceARN)
		if err == nil {
			tags = w.Tags
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	list := tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
	selected, next, err := page(list, func(t api.Tag) string { return *t.Key }, in.NextToken, in.MaxResults, 100, 100)
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceOutput{Tags: append([]api.Tag{}, selected...), NextToken: next}, nil
}
