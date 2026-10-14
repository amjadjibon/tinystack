package timestreamwrite

import (
	"context"
	"errors"
	"maps"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamwrite"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagError(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return fault("ServiceQuotaExceededException", "%s", err.Error())
	}

	return invalid("%s", err.Error())
}

// tagMap reads tags given at creation or by TagResource.
func tagMap(tags []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(tags, func(t api.Tag) (*string, *string) { return t.Key, t.Value }, nil, nil)
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(out); err != nil {
		return nil, tagError(err)
	}

	return out, nil
}

func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
}

// tagSet holds a resource's tags. Every stored resource embeds it, so its tags stay in the resource's own record.
type tagSet struct {
	Tags map[string]string
}

func (t *tagSet) tagMap() map[string]string        { return t.Tags }
func (t *tagSet) setTagMap(tags map[string]string) { t.Tags = tags }

type tagged interface {
	tagMap() map[string]string
	setTagMap(map[string]string)
}

// reference is the database or table an ARN names.
type reference struct {
	kind, database, table string
}

func (r reference) key(scope request.Scope) string {
	if r.kind == kindDatabase {
		return key(scope, kindDatabase, r.database)
	}

	return tableKey(scope, r.database, r.table)
}

func (r reference) notFound() error {
	if r.kind == kindDatabase {
		return databaseNotFound(r.database)
	}

	return tableNotFound(r.database, r.table)
}

// parseARN reads the ARN of a database or table in the request's account and Region.
func parseARN(scope request.Scope, value *string) (reference, error) {
	bad := invalid("%q is not a valid Timestream ARN.", common.Value(value))
	if value == nil {
		return reference{}, bad
	}

	parts := strings.SplitN(*value, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "timestream" {
		return reference{}, bad
	}

	segments := strings.Split(parts[5], "/")
	var ref reference
	switch {
	case len(segments) == 2 && segments[0] == kindDatabase:
		ref = reference{kind: kindDatabase, database: segments[1]}
	case len(segments) == 4 && segments[0] == kindDatabase && segments[2] == kindTable:
		ref = reference{kind: kindTable, database: segments[1], table: segments[3]}
	default:
		return reference{}, bad
	}

	if parts[3] != scope.Region || parts[4] != scope.AccountID {
		return reference{}, ref.notFound()
	}

	return ref, nil
}

// editTags applies change to the tags of the resource an ARN names, in one transaction, and returns the result.
func (s *Service) editTags(
	ctx context.Context,
	value *string,
	change func(tags map[string]string) (map[string]string, error),
) (map[string]string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	ref, err := parseARN(scope, value)
	if err != nil {
		return nil, err
	}

	var out map[string]string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var r tagged
		var found bool
		var e error
		if ref.kind == kindDatabase {
			r, found, e = readTagged[databaseRecord](tx, ref.key(scope))
		} else {
			r, found, e = readTagged[tableRecord](tx, ref.key(scope))
		}

		if e != nil {
			return e
		}

		if !found {
			return ref.notFound()
		}

		if out, e = change(maps.Clone(r.tagMap())); e != nil {
			return e
		}

		r.setTagMap(out)
		return storage.Write(tx, ref.key(scope), r)
	})

	return out, err
}

func readTagged[T any, P interface {
	*T
	tagged
}](tx storage.ReadTx, k string) (tagged, bool, error) {
	r, found, err := storage.Read[T](tx, k)
	if err != nil || !found {
		return nil, found, err
	}

	return P(r), true, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	tags, err := s.editTags(ctx, in.ResourceARN, func(tags map[string]string) (map[string]string, error) {
		return tags, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{Tags: tagList(tags)}, nil
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if len(add) == 0 {
		return nil, invalid("Tags is required.")
	}

	_, err = s.editTags(ctx, in.ResourceARN, func(current map[string]string) (map[string]string, error) {
		merged, e := tagging.Merge(current, add)
		if e != nil {
			return nil, tagError(e)
		}

		return merged, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	if len(in.TagKeys) == 0 {
		return nil, invalid("TagKeys is required.")
	}

	_, err := s.editTags(ctx, in.ResourceARN, func(current map[string]string) (map[string]string, error) {
		return tagging.Remove(current, in.TagKeys), nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{}, nil
}
