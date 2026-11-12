package memorydb

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/memorydb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagError(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return fault("TagQuotaPerResourceExceeded", "%s", err.Error())
	}

	return invalidValue("%s", err.Error())
}

// tagMap reads tags given at creation. A tag's value may be null.
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

// tagged is a stored resource that carries tags.
type tagged interface {
	tagMap() map[string]string
	setTagMap(map[string]string)
}

// reference is a resource named by an ARN.
type reference struct {
	kind, name string
}

func (r reference) notFound() error {
	switch r.kind {
	case kindCluster:
		return clusterNotFound(r.name)
	case kindUser:
		return userNotFound(r.name)
	case kindACL:
		return aclNotFound(r.name)
	case kindParameterGroup:
		return parameterGroupNotFound(r.name)
	default:
		return subnetGroupNotFound(r.name)
	}
}

// parseARN reads the ARN of a resource in the request's account and Region.
func parseARN(scope request.Scope, value *string) (reference, error) {
	invalid := fault("InvalidARNFault", "Invalid ARN %s", sanitize(value))
	if value == nil {
		return reference{}, invalid
	}

	tail, ok := strings.CutPrefix(*value, "arn:aws:memorydb:")
	if !ok {
		return reference{}, invalid
	}

	parts := strings.SplitN(tail, ":", 3)
	if len(parts) != 3 {
		return reference{}, invalid
	}

	kind, name, ok := strings.Cut(parts[2], "/")
	if !ok || name == "" ||
		!slices.Contains([]string{kindCluster, kindUser, kindACL, kindSubnetGroup, kindParameterGroup}, kind) {
		return reference{}, invalid
	}

	ref := reference{kind: kind, name: name}
	if parts[0] != scope.Region || parts[1] != scope.AccountID {
		return reference{}, ref.notFound()
	}

	return ref, nil
}

func sanitize(value *string) string {
	if value == nil {
		return ""
	}

	return fmt.Sprintf("%q", *value)
}

func (r reference) key(scope request.Scope) string { return key(scope, r.kind, r.name) }

// builtIn reports whether the resource is the default user or the open-access ACL, which cannot carry tags.
func (r reference) builtIn() bool {
	return r.kind == kindUser && r.name == defaultUserName || r.kind == kindACL && r.name == openAccessACL ||
		r.kind == kindParameterGroup &&
			slices.ContainsFunc(parameterFamilies, func(f string) bool { return defaultGroup(f) == r.name })
}

// readTags returns a resource's tags.
func readTags(tx storage.ReadTx, scope request.Scope, ref reference) (map[string]string, error) {
	if ref.builtIn() {
		return map[string]string{}, nil
	}

	var r tagged
	var found bool
	var err error
	switch ref.kind {
	case kindParameterGroup:
		r, found, err = readTagged[parameterGroupRecord](tx, ref.key(scope))
	case kindCluster:
		r, found, err = readTagged[clusterRecord](tx, ref.key(scope))
	case kindUser:
		r, found, err = readTagged[userRecord](tx, ref.key(scope))
	case kindACL:
		r, found, err = readTagged[aclRecord](tx, ref.key(scope))
	default:
		r, found, err = readTagged[subnetGroupRecord](tx, ref.key(scope))
	}

	if err != nil {
		return nil, err
	}

	if !found {
		return nil, ref.notFound()
	}

	return r.tagMap(), nil
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

// writeTags replaces a resource's tags.
func writeTags(tx storage.WriteTx, scope request.Scope, ref reference, tags map[string]string) error {
	var err error
	switch ref.kind {
	case kindParameterGroup:
		err = replaceTags[parameterGroupRecord](tx, ref.key(scope), tags)
	case kindCluster:
		err = replaceTags[clusterRecord](tx, ref.key(scope), tags)
	case kindUser:
		err = replaceTags[userRecord](tx, ref.key(scope), tags)
	case kindACL:
		err = replaceTags[aclRecord](tx, ref.key(scope), tags)
	default:
		err = replaceTags[subnetGroupRecord](tx, ref.key(scope), tags)
	}

	return err
}

func replaceTags[T any, P interface {
	*T
	tagged
}](tx storage.WriteTx, k string, tags map[string]string) error {
	r, _, err := storage.Read[T](tx, k)
	if err != nil {
		return err
	}

	P(r).setTagMap(tags)
	return storage.Write(tx, k, r)
}

func (s *Service) ListTags(ctx context.Context, in *api.ListTagsRequest) (*api.ListTagsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	ref, err := parseARN(scope, in.ResourceArn)
	if err != nil {
		return nil, err
	}

	var tags map[string]string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		tags, e = readTags(tx, scope, ref)
		return e
	})
	if err != nil {
		return nil, err
	}

	return &api.ListTagsResponse{TagList: tagList(tags)}, nil
}

// changeTags applies change to a resource's tags in one transaction and returns the result.
func (s *Service) changeTags(
	ctx context.Context,
	arn *string,
	change func(tags map[string]string) (map[string]string, error),
) ([]api.Tag, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	ref, err := parseARN(scope, arn)
	if err != nil {
		return nil, err
	}

	var out map[string]string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		current, e := readTags(tx, scope, ref)
		if e != nil {
			return e
		}

		if ref.builtIn() {
			return invalidValue("The built-in %s %s cannot be tagged.", ref.kind, ref.name)
		}

		if out, e = change(current); e != nil {
			return e
		}

		return writeTags(tx, scope, ref, out)
	})
	if err != nil {
		return nil, err
	}

	return tagList(out), nil
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if len(add) == 0 {
		return nil, invalidValue("Tags is required.")
	}

	tags, err := s.changeTags(ctx, in.ResourceArn, func(current map[string]string) (map[string]string, error) {
		merged, e := tagging.Merge(current, add)
		if e != nil {
			return nil, tagError(e)
		}

		return merged, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{TagList: tags}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	if len(in.TagKeys) == 0 {
		return nil, invalidValue("TagKeys is required.")
	}

	tags, err := s.changeTags(ctx, in.ResourceArn, func(current map[string]string) (map[string]string, error) {
		for _, k := range in.TagKeys {
			if _, ok := current[k]; !ok {
				return nil, fault("TagNotFoundFault", "The tag %s does not exist on the resource.", k)
			}
		}

		return tagging.Remove(maps.Clone(current), in.TagKeys), nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{TagList: tags}, nil
}
