// Package tagging holds the resource tag rules that AWS services share. Each
// service stores tags as a map and reports violations with its own error code.
package tagging

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/common"
)

// Max is the number of tags one resource may have.
const Max = 50

// ErrLimit reports a tag set larger than Max. Some services give it its own
// error code.
var ErrLimit = fmt.Errorf("a resource can have at most %d tags", Max)

// ErrReserved reports a key that is Reserved.
var ErrReserved = errors.New("tag keys cannot start with the reserved prefix aws")

// Reserved reports whether key has the aws: prefix, which AWS keeps for its own tags.
func Reserved(key string) bool { return strings.HasPrefix(strings.ToLower(key), "aws:") }

// CheckSet is Check for tags a caller sets, whose keys must not be Reserved.
func CheckSet(tags map[string]string) error {
	for k := range tags {
		if Reserved(k) {
			return ErrReserved
		}
	}

	return Check(tags)
}

// Check reports the first way tags break the shared limits: at most Max tags,
// keys of 1 to 128 characters and values of at most 256.
func Check(tags map[string]string) error {
	if len(tags) > Max {
		return ErrLimit
	}

	for k, v := range tags {
		if n := utf8.RuneCountInString(k); n < 1 || n > 128 {
			return errors.New("tag keys must be 1 to 128 characters long")
		}

		if utf8.RuneCountInString(v) > 256 {
			return errors.New("tag values must be at most 256 characters long")
		}
	}

	return nil
}

// Merge returns current with add applied, checked against the limits. It
// leaves current unchanged.
func Merge(current, add map[string]string) (map[string]string, error) {
	tags := make(map[string]string, len(current)+len(add))
	maps.Copy(tags, current)
	maps.Copy(tags, add)
	if err := Check(tags); err != nil {
		return nil, err
	}

	return tags, nil
}

// Remove deletes keys from tags and returns tags.
func Remove(tags map[string]string, keys []string) map[string]string {
	for _, k := range keys {
		delete(tags, k)
	}

	return tags
}

// Map returns a service's tag list as a map, reading each tag's key and value with pair. A tag without its key or
// value fails with missing, and a repeated key with duplicate(key). A nil missing reads an absent key or value as
// empty, and a nil duplicate lets a later tag replace an earlier one.
func Map[T any](tags []T, pair func(T) (key, value *string), missing error, duplicate func(key string) error) (
	map[string]string, error,
) {
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		k, v := pair(t)
		if missing != nil && (k == nil || v == nil) {
			return nil, missing
		}

		if _, dup := out[common.Value(k)]; dup && duplicate != nil {
			return nil, duplicate(common.Value(k))
		}

		out[common.Value(k)] = common.Value(v)
	}

	return out, nil
}

// List returns tags as a service's tag list, ordered by key and built with tag. It is never nil.
func List[T any](tags map[string]string, tag func(key, value string) T) []T {
	out := make([]T, 0, len(tags))
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		out = append(out, tag(k, tags[k]))
	}

	return out
}
