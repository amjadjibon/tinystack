package ec2

import (
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
)

func TestWildcard(t *testing.T) {
	for _, tc := range []struct {
		pattern, text string
		want          bool
	}{
		{"web-*", "web-prod", true},
		{"web-*", "web", false},
		{"?b", "ab", true},
		{"?b", "aab", false},
		{"a.b", "axb", false},
		{`a\*`, "a*", true},
		{`a\*`, "ab", false},
		{`a\?`, "a?", true},
		{`a\\`, `a\`, true},
		{"", "", true},
	} {
		if got := wildcard(tc.pattern).MatchString(tc.text); got != tc.want {
			t.Errorf("%q on %q = %v", tc.pattern, tc.text, got)
		}
	}
}

func TestFilters(t *testing.T) {
	v := values{"state": {"available"}}.withTags(map[string]string{"Name": "web", "team": "core"})
	for _, tc := range []struct {
		filters []api.Filter
		want    bool
	}{
		{nil, true},
		{[]api.Filter{{Name: new("state"), Values: []string{"pending", "available"}}}, true},
		{[]api.Filter{
			{Name: new("state"), Values: []string{"available"}}, {Name: new("tag:Name"), Values: []string{"db"}},
		}, false},
		{[]api.Filter{{Name: new("tag-value"), Values: []string{"co*"}}}, true},
		{[]api.Filter{{Name: new("tag:missing"), Values: []string{"*"}}}, false},
	} {
		fs, err := compileFilters(tc.filters, "state")
		if err != nil {
			t.Fatal(err)
		}

		if got := fs.match(v); got != tc.want {
			t.Errorf("%+v: got %v", tc.filters, got)
		}
	}

	for _, name := range []string{"color", "tag:", "State"} {
		if _, err := compileFilters([]api.Filter{{Name: new(name)}}, "state"); err == nil {
			t.Errorf("accepted filter %q", name)
		}
	}
}
