package iotwireless

import (
	"errors"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iotwireless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// tagsOf reads the tags of a create request.
func tagsOf(tags []api.Tag) (map[string]string, error) {
	out, _ := tagging.Map(tags, func(t api.Tag) (key, value *string) { return t.Key, t.Value }, nil, nil)
	if err := tagging.CheckSet(out); err != nil {
		if errors.Is(err, tagging.ErrLimit) {
			return nil, awserror.New("TooManyTagsException", err.Error())
		}

		return nil, invalid("%v", err)
	}

	return out, nil
}

// tagList renders tags as the API's tag list.
func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(key, value string) api.Tag { return api.Tag{Key: new(key), Value: new(value)} })
}

// indexOf reads an index entry.
func indexOf(tx storage.ReadTx, scope request.Scope, kind, name string) (string, bool) {
	v, ok := tx.Get(scope.Key(serviceName, kind, name))
	return string(v), ok
}

// claim points an index entry at its owner, unless another owner holds it.
func claim(tx storage.WriteTx, scope request.Scope, kind, name, owner string) error {
	if held, taken := indexOf(tx, scope, kind, name); taken && held != owner {
		return conflict("%s %s is already in use", kind, name)
	}

	tx.Put(scope.Key(serviceName, kind, name), []byte(owner))
	return nil
}

// newRecord makes a record for a new resource with a generated ID.
func newRecord(scope request.Scope, kind, id string, tags map[string]string) *record {
	return &record{Kind: kind, ID: id, Arn: arn(scope, kind, id), Tags: common.CloneTags(tags)}
}
