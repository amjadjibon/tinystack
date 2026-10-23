// Package validation checks decoded inputs against the Smithy constraint traits (required, length, range, pattern
// and enum) that the generator emits for services whose profile opts in. Messages follow the AWS wording.
package validation

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Error is the first input value that breaks a model constraint.
type Error struct {
	Path       string
	Constraint string
}

func (e *Error) Error() string {
	return "1 validation error detected: Value at '" + e.Path + "' failed to satisfy constraint: Member must " +
		e.Constraint
}

func fail(path, constraint string) error { return &Error{Path: path, Constraint: constraint} }

// Index names a list element.
func Index(path string, i int) string { return path + "[" + strconv.Itoa(i) + "]" }

// Key names a map entry.
func Key(path, key string) string { return path + "[" + key + "]" }

// Required fails for a missing member.
func Required(path string, present bool) error {
	if !present {
		return fail(path, "not be null")
	}

	return nil
}

// Length checks a string's code points, a blob's bytes, or a list's or map's entries.
func Length(path string, n int, minimum, maximum *int64) error {
	switch {
	case minimum != nil && int64(n) < *minimum:
		return fail(path, fmt.Sprintf("have length greater than or equal to %d", *minimum))
	case maximum != nil && int64(n) > *maximum:
		return fail(path, fmt.Sprintf("have length less than or equal to %d", *maximum))
	}

	return nil
}

// Range checks a number.
func Range(path string, v float64, minimum, maximum *float64) error {
	switch {
	case minimum != nil && v < *minimum:
		return fail(path, "have value greater than or equal to "+strconv.FormatFloat(*minimum, 'f', -1, 64))
	case maximum != nil && v > *maximum:
		return fail(path, "have value less than or equal to "+strconv.FormatFloat(*maximum, 'f', -1, 64))
	}

	return nil
}

// Pattern checks a string against the model pattern, which source holds as written. A leading negative lookahead
// such as ^(?!aws:), which Go's regexp cannot express, is passed as excluded and checked as a prefix.
func Pattern(path, v string, re *regexp.Regexp, excluded, source string) error {
	if (excluded != "" && strings.HasPrefix(v, excluded)) || !re.MatchString(v) {
		return fail(path, "satisfy regular expression pattern: "+source)
	}

	return nil
}

// Enum checks a string against the model's values.
func Enum(path, v string, values []string) error {
	if !slices.Contains(values, v) {
		return fail(path, "satisfy enum value set: ["+strings.Join(values, ", ")+"]")
	}

	return nil
}
