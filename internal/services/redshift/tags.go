package redshift

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/redshift"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagError(err error) error {
	code := "InvalidTagFault"
	if errors.Is(err, tagging.ErrLimit) {
		code = "TagLimitExceededFault"
	}

	return awserror.New(code, err.Error())
}

func tagMap(tags []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(tags, func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		awserror.New("InvalidTagFault", "Tag key and value are required"),
		func(k string) error { return awserror.New("InvalidTagFault", "Duplicate tag key: "+k) })
	if err != nil {
		return nil, err
	}

	if err := tagging.CheckSet(out); err != nil {
		return nil, tagError(err)
	}

	return out, nil
}

func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
}

func readTagged(tx storage.ReadTx, scope request.Scope, value *string) (*record, error) {
	kind, name, err := resourceReference(scope, value)
	if err != nil {
		return nil, err
	}

	r, err := read(tx, scope, kind, name)
	if err != nil {
		return nil, awserror.New("ResourceNotFoundFault", "Redshift resource does not exist")
	}

	return r, nil
}

func (s *Service) changeTags(ctx context.Context, resource *string, add map[string]string, remove []string) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readTagged(tx, scope, resource)
		if e != nil {
			return e
		}

		if r.Kind == parameterKind && r.Name == defaultParameter {
			return invalid("Built-in parameter group tags are immutable")
		}

		tags, e := tagging.Merge(r.Tags, add)
		if e != nil {
			return tagError(e)
		}

		r.Tags = tagging.Remove(tags, remove)
		return save(tx, scope, r)
	})
}

func (s *Service) CreateTags(ctx context.Context, in *api.CreateTagsMessage) (*api.CreateTagsOutput, error) {
	if in == nil || len(in.Tags) == 0 {
		return nil, awserror.New("InvalidTagFault", "Tags are required")
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.changeTags(ctx, in.ResourceName, tags, nil)
	return &api.CreateTagsOutput{}, err
}

func (s *Service) DeleteTags(ctx context.Context, in *api.DeleteTagsMessage) (*api.DeleteTagsOutput, error) {
	if in == nil || len(in.TagKeys) == 0 {
		return nil, awserror.New("InvalidTagFault", "TagKeys are required")
	}

	keys := map[string]string{}
	for _, k := range in.TagKeys {
		keys[k] = ""
	}

	if err := tagging.CheckSet(keys); err != nil {
		return nil, tagError(err)
	}

	err := s.changeTags(ctx, in.ResourceName, nil, slices.Clone(in.TagKeys))
	return &api.DeleteTagsOutput{}, err
}

func (s *Service) DescribeTags(
	ctx context.Context,
	in *api.DescribeTagsMessage,
) (*api.TaggedResourceListMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalid("Input is required")
	}

	limit, err := pageSize(in.MaxRecords)
	if err != nil {
		return nil, err
	}

	if in.ResourceType != nil && !slices.Contains([]string{clusterKind, subnetKind, parameterKind}, *in.ResourceType) {
		return nil, awserror.New("NotImplemented", "Only cluster, subnetgroup and parametergroup tags are supported")
	}

	rows := []api.TaggedResource{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		items := []*record{}
		if in.ResourceName != nil {
			r, e := readTagged(tx, scope, in.ResourceName)
			if e != nil {
				return e
			}

			items = append(items, r)
		} else {
			// Kind order is the storage prefix order; each prefix returns sorted keys.
			for _, kind := range []string{clusterKind, parameterKind, subnetKind} {
				if in.ResourceType != nil && *in.ResourceType != kind {
					continue
				}

				selected, e := storage.Records[record](tx, scope.Prefix(serviceName, kind))
				if e != nil {
					return e
				}

				items = append(items, selected...)
			}
		}

		for _, r := range items {
			if in.ResourceType != nil && *in.ResourceType != r.Kind {
				continue
			}

			for _, key := range slices.Sorted(maps.Keys(r.Tags)) {
				rows = append(rows, api.TaggedResource{
					ResourceName: new(arn(scope, r.Kind, r.Name)),
					ResourceType: new(r.Kind), Tag: &api.Tag{Key: new(key), Value: new(r.Tags[key])},
				})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	rows, marker, err := common.Page(rows, func(r api.TaggedResource) string {
		return fmt.Sprintf("%s\x00%s", *r.ResourceName, *r.Tag.Key)
	}, in.Marker, limit, invalid("Invalid Marker"))
	return &api.TaggedResourceListMessage{TaggedResources: rows, Marker: marker}, err
}
