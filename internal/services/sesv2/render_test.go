package sesv2

import (
	"encoding/json"
	"testing"
)

func TestRender(t *testing.T) {
	var data any
	if err := json.Unmarshal([]byte(`{"name":"Ana","n":0,"user":{"city":"Oslo"},
		"orders":[{"id":1,"items":["a","b"]},{"id":2,"items":[]}]}`), &data); err != nil {
		t.Fatal(err)
	}

	for src, want := range map[string]string{
		"{{name}} {{user.city}}":                                                     "Ana Oslo",
		"{{#if n}}yes{{else}}no{{/if}}{{#unless n}}!{{/unless}}":                     "no!",
		"{{#with user}}{{city}}/{{../name}}{{/with}}":                                "Oslo/Ana",
		"{{#each orders}}{{id}}:{{#each items}}{{this}}{{else}}-{{/each}};{{/each}}": "1:ab;2:-;",
		"{{#each orders}}{{#if @first}}first {{/if}}{{@index}}{{/each}}":             "first 01",
		"{{! a comment }}{{!-- {{name}} --}}x":                                       "x",
	} {
		nodes, err := parseTemplate(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}

		if got, err := render(nodes, data, false); err != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", src, got, err, want)
		}
	}

	for _, src := range []string{
		"{{#if a}}", "{{/if}}", "{{else}}", "{{#if a}}{{/each}}", "{{#lookup a}}{{/lookup}}",
		"{{name", "{{format name}}",
	} {
		if _, err := parseTemplate(src); err == nil {
			t.Errorf("%s parsed", src)
		}
	}

	nodes, _ := parseTemplate("{{missing}}")
	if _, err := render(
		nodes,
		data,
		false,
	); err == nil ||
		err.Error() != "Attribute 'missing' is not present in the rendering data." {
		t.Fatalf("missing = %v", err)
	}
}
