// Package tagging holds the resource tag rules that AWS services share. Each
// service stores tags as a map and reports violations with its own error code.
package tagging

import (
	"errors"
	"fmt"
	"maps"
	"unicode/utf8"
)

// Max is the number of tags one resource may have.
const Max = 50

// ErrLimit reports a tag set larger than Max. Some services give it its own
// error code.
var ErrLimit = fmt.Errorf("a resource can have at most %d tags", Max)

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
