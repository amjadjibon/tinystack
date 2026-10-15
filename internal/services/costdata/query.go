package costdata

import (
	"fmt"
	"slices"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/costexplorer"
	"github.com/amjadjibon/tinystack/internal/common"
)

// Predicate validates the supported expression before returning its matcher.
func Predicate(e *api.Expression) (func(Line) bool, error) {
	return predicate(e, 0)
}

func predicate(e *api.Expression, depth int) (func(Line) bool, error) {
	if e == nil {
		return func(Line) bool { return true }, nil
	}

	if e.CostCategories != nil {
		return nil, fmt.Errorf("CostCategories filters are not supported")
	}

	if depth > 32 {
		return nil, fmt.Errorf("filter exceeds 32 levels")
	}

	count := 0
	for _, present := range []bool{e.And != nil, e.Or != nil, e.Not != nil, e.Dimensions != nil, e.Tags != nil} {
		if present {
			count++
		}
	}

	if count != 1 {
		return nil, fmt.Errorf("expression must contain exactly one operator")
	}

	if e.Dimensions != nil || e.Tags != nil {
		var key string
		var values, options []string
		tag := e.Tags != nil
		if tag {
			key = common.Value(e.Tags.Key)
			values = e.Tags.Values
			options = e.Tags.MatchOptions
		} else {
			key = common.Value(e.Dimensions.Key)
			values = e.Dimensions.Values
			options = e.Dimensions.MatchOptions
			if _, ok := Dimension(&Line{}, key); !ok {
				return nil, fmt.Errorf("unsupported dimension %s", key)
			}
		}

		if key == "" || len(values) == 0 {
			return nil, fmt.Errorf("filter key and values are required")
		}

		if len(options) > 1 || len(options) == 1 && options[0] != "EQUALS" {
			return nil, fmt.Errorf("only EQUALS MatchOptions is supported")
		}

		return func(l Line) bool {
			value, exists := Dimension(&l, key)
			if tag {
				value, exists = l.Tags[key]
			}

			return exists && slices.Contains(values, value)
		}, nil
	}

	if e.Not != nil {
		p, err := predicate(e.Not, depth+1)
		if err != nil {
			return nil, err
		}

		return func(l Line) bool { return !p(l) }, nil
	}

	children := e.And
	if e.Or != nil {
		children = e.Or
	}

	if len(children) == 0 {
		return nil, fmt.Errorf("And/Or requires children")
	}

	var predicates []func(Line) bool
	for i := range children {
		p, err := predicate(&children[i], depth+1)
		if err != nil {
			return nil, err
		}

		predicates = append(predicates, p)
	}

	return func(l Line) bool {
		for _, p := range predicates {
			if e.Or != nil && p(l) {
				return true
			}

			if e.And != nil && !p(l) {
				return false
			}
		}

		return e.And != nil
	}, nil
}

func Select(lines []Line, start, end time.Time, match func(Line) bool) []Line {
	var selected []Line
	for i := range lines {
		l := &lines[i]
		t, _ := ParseTime(l.Start)
		if !t.Before(start) && t.Before(end) && match(*l) {
			selected = append(selected, *l)
		}
	}

	return selected
}

// ByPeriod buckets selected lines once, attributing each whole amount to its start date.
// The first monthly bucket is clipped to the query's inclusive start.
func ByPeriod(lines []Line, start time.Time, monthly bool) map[string][]Line {
	periods := make(map[string][]Line)
	for i := range lines {
		line := &lines[i]
		t, _ := ParseTime(line.Start)
		if monthly {
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
			if t.Before(start) {
				t = start
			}
		}

		key := t.Format("2006-01-02")
		periods[key] = append(periods[key], *line)
	}

	return periods
}
