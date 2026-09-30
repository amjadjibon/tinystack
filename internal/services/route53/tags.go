package route53

import (
	"context"
	"maps"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/route53"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	tagZone        = "hostedzone"
	tagHealthCheck = "healthcheck"
	maxTagChanges  = 10
)

// loadTags reads the tags of a hosted zone or health check.
func loadTags(tx storage.ReadTx, account string, kind, id *string) (map[string]string, error) {
	switch value(kind) {
	case tagZone:
		z, err := loadZone(tx, account, bareID(id))
		if err != nil {
			return nil, err
		}

		return z.Tags, nil
	case tagHealthCheck:
		h, err := loadHealthCheck(tx, account, value(id))
		if err != nil {
			return nil, err
		}

		return h.Tags, nil
	}

	return nil, invalid("The ResourceType must be hostedzone or healthcheck.")
}

func tagSet(kind, id *string, tags map[string]string) api.ResourceTagSet {
	set := api.ResourceTagSet{ResourceType: kind, ResourceId: new(bareID(id)), Tags: []api.Tag{}}
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		set.Tags = append(set.Tags, api.Tag{Key: new(k), Value: new(tags[k])})
	}

	return set
}

func (s *Service) ChangeTagsForResource(
	ctx context.Context, in *api.ChangeTagsForResourceRequest,
) (*api.ChangeTagsForResourceResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.AddTags) > maxTagChanges || len(in.RemoveTagKeys) > maxTagChanges {
		return nil, invalid("A request can add and remove at most %d tags each.", maxTagChanges)
	}

	add := map[string]string{}
	for _, t := range in.AddTags {
		add[value(t.Key)] = value(t.Value)
	}

	return &api.ChangeTagsForResourceResponse{}, s.store.Update(ctx, func(tx storage.WriteTx) error {
		tags, loadErr := loadTags(tx, m.AccountID, in.ResourceType, in.ResourceId)
		if loadErr != nil {
			return loadErr
		}

		tags, loadErr = tagging.Merge(tagging.Remove(tags, in.RemoveTagKeys), add)
		if loadErr != nil {
			return invalid("%s", loadErr.Error())
		}

		if value(in.ResourceType) == tagZone {
			z, _ := loadZone(tx, m.AccountID, bareID(in.ResourceId))
			z.Tags = tags
			return write(tx, key(m.AccountID, kindZone, bareID(in.ResourceId)), z)
		}

		h, _ := loadHealthCheck(tx, m.AccountID, value(in.ResourceId))
		h.Tags = tags
		return write(tx, key(m.AccountID, kindHealthCheck, value(in.ResourceId)), h)
	})
}

func (s *Service) ListTagsForResource(
	ctx context.Context, in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var tags map[string]string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		tags, err = loadTags(tx, m.AccountID, in.ResourceType, in.ResourceId)
		return err
	})
	if err != nil {
		return nil, err
	}

	set := tagSet(in.ResourceType, in.ResourceId, tags)
	return &api.ListTagsForResourceResponse{ResourceTagSet: &set}, nil
}

func (s *Service) ListTagsForResources(
	ctx context.Context, in *api.ListTagsForResourcesRequest,
) (*api.ListTagsForResourcesResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.ResourceIds) == 0 || len(in.ResourceIds) > maxTagChanges {
		return nil, invalid("ResourceIds must have 1 to %d IDs.", maxTagChanges)
	}

	out := &api.ListTagsForResourcesResponse{ResourceTagSets: []api.ResourceTagSet{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, id := range in.ResourceIds {
			tags, loadErr := loadTags(tx, m.AccountID, in.ResourceType, &id)
			if loadErr != nil {
				return loadErr
			}

			out.ResourceTagSets = append(out.ResourceTagSets, tagSet(in.ResourceType, &id, tags))
		}

		return nil
	})

	return out, err
}
