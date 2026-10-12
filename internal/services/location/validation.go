package location

import (
	"regexp"
	"strings"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/location"
	"github.com/amjadjibon/tinystack/internal/common"
)

var (
	namePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	tagPattern  = regexp.MustCompile(`^[\p{L}\p{Z}\p{N}_.,:/=+\-@]*$`)
)

func checkKey(key string) error {
	if !utf8.ValidString(key) || utf8.RuneCountInString(key) < 1 || utf8.RuneCountInString(key) > 128 ||
		!tagPattern.MatchString(key) ||
		strings.HasPrefix(strings.ToLower(key), "aws:") {
		return invalid("Tag keys require 1-128 permitted Unicode characters and cannot start with aws:")
	}
	return nil
}

func checkTags(tags map[string]string) error {
	if len(tags) > 50 {
		return invalid("A tracker supports at most 50 tags")
	}
	for k, v := range tags {
		if e := checkKey(k); e != nil {
			return e
		}
		if !utf8.ValidString(v) || utf8.RuneCountInString(v) > 256 || !tagPattern.MatchString(v) {
			return invalid("Tag values require 0-256 permitted Unicode characters")
		}
	}
	return nil
}

// ValidateConfiguration checks a complete selected tracker configuration.
func ValidateConfiguration(t *api.DescribeTrackerResponse) error {
	if t == nil || !namePattern.MatchString(common.Value(t.TrackerName)) {
		return invalid("TrackerName requires 1-100 ASCII letters, digits, -, . or _")
	}
	if t.Description == nil || !utf8.ValidString(*t.Description) || utf8.RuneCountInString(*t.Description) > 1000 {
		return invalid("Description requires 0-1000 Unicode characters")
	}
	switch common.Value(t.PositionFiltering) {
	case "TimeBased", "DistanceBased", "AccuracyBased":
	default:
		return invalid("PositionFiltering requires TimeBased, DistanceBased or AccuracyBased")
	}
	return checkTags(t.Tags)
}
