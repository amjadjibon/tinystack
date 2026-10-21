// Package wafcommon shares tag validation between WAF Classic and WAFv2.
package wafcommon

import (
	"errors"
	"regexp"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

var tagPattern = regexp.MustCompile(`^[\p{L}\p{Z}\p{N}_.:/=+\-@]*$`)

func invalid(message string) error {
	e := awserror.New("WAFInvalidParameterException", message)
	e.Fields = map[string]any{"Reason": message}
	return e
}

// TagError maps a tagging failure to the WAF limit or parameter error.
func TagError(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return awserror.New("WAFLimitsExceededException", err.Error())
	}

	return invalid(err.Error())
}

// Tags validates WAF tags and returns them as a map; required rejects an absent list.
func Tags[T any](tags []T, required bool, fields func(T) (*string, *string)) (map[string]string, error) {
	if (required || tags != nil) && len(tags) == 0 {
		return nil, invalid("Tags must contain at least one tag")
	}

	out, err := tagging.Map(tags, fields, invalid("Tags require Key and Value"),
		func(string) error { return invalid("Tag keys must be unique") })
	if err != nil {
		return nil, err
	}

	for k, v := range out {
		if !tagPattern.MatchString(k) || !tagPattern.MatchString(v) {
			return nil, invalid("Invalid tag character")
		}
	}

	if err = tagging.CheckSet(out); err != nil {
		return nil, TagError(err)
	}

	return out, nil
}
