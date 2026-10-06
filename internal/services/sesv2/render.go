package sesv2

import (
	"errors"
	"fmt"
	"html"
	"strconv"
	"strings"
)

// node is a parsed part of a template: text, a {{value}} ({{{value}}}
// unescaped), or a block ({{#if}}, {{#unless}}, {{#each}}, {{#with}}) with
// its body and its {{else}} part.
type node struct {
	text    string
	path    string
	block   string
	body    []node
	inverse []node
	raw     bool
}

const (
	tagElse  = "else"
	pathThis = "this"
)

// blocks are the block helpers SES templates may use.
var blocks = map[string]bool{"if": true, "unless": true, "each": true, "with": true}

// parseTemplate parses the subset of Handlebars SES templates use.
func parseTemplate(src string) ([]node, error) {
	nodes, _, stop, err := parseNodes(src)
	switch {
	case err != nil:
		return nil, err
	case stop == tagElse:
		return nil, errors.New("{{else}} is outside a block")
	case stop != "":
		return nil, fmt.Errorf("{{%s}} closes no block", stop)
	}

	return nodes, nil
}

// parseNodes parses src until its end, an {{else}}, or a closing tag, which
// stop reports ("", "else", or "/<name>"), and returns what follows.
func parseNodes(src string) (nodes []node, rest, stop string, err error) {
	for src != "" {
		start := strings.Index(src, "{{")
		if start < 0 {
			return append(nodes, node{text: src}), "", "", nil
		}

		if start > 0 {
			nodes = append(nodes, node{text: src[:start]})
		}

		src = src[start:]
		raw := strings.HasPrefix(src, "{{{")
		open, end := 2, "}}"
		switch {
		case raw:
			open, end = 3, "}}}"
		case strings.HasPrefix(src, "{{!--"):
			end = "--}}"
		}

		closing := strings.Index(src[open:], end)
		if closing < 0 {
			return nil, "", "", errors.New("a {{ tag is not closed")
		}

		tag := strings.TrimSpace(src[open : open+closing])
		src = src[open+closing+len(end):]
		switch {
		case strings.HasPrefix(tag, "!"):
		case raw:
			nodes = append(nodes, node{path: tag, raw: true})
		case tag == tagElse:
			return nodes, src, tagElse, nil
		case strings.HasPrefix(tag, "/"):
			return nodes, src, "/" + strings.TrimSpace(tag[1:]), nil
		case strings.HasPrefix(tag, "#"):
			name, arg, _ := strings.Cut(strings.TrimSpace(tag[1:]), " ")
			if !blocks[name] {
				return nil, "", "", fmt.Errorf("TinyStack does not support the %s helper", name)
			}

			b := node{block: name, path: strings.TrimSpace(arg)}
			b.body, src, stop, err = parseNodes(src)
			if err == nil && stop == tagElse {
				b.inverse, src, stop, err = parseNodes(src)
			}

			switch {
			case err != nil:
				return nil, "", "", err
			case stop != "/"+name:
				return nil, "", "", fmt.Errorf("{{#%s}} is not closed", name)
			}

			nodes = append(nodes, b)
		case strings.ContainsAny(tag, " \t"):
			return nil, "", "", fmt.Errorf("TinyStack does not support the {{%s}} expression", tag)
		default:
			nodes = append(nodes, node{path: tag})
		}
	}

	return nodes, "", "", nil
}

// missingError is a {{value}} the rendering data lacks.
type missingError struct{ name string }

func (e *missingError) Error() string {
	return fmt.Sprintf("Attribute '%s' is not present in the rendering data.", e.name)
}

// render renders nodes with data, escaping values for HTML when escape is
// set.
func render(nodes []node, data any, escape bool) (string, error) {
	var b strings.Builder
	err := renderTo(&b, nodes, []any{data}, escape)
	return b.String(), err
}

func renderTo(b *strings.Builder, nodes []node, scopes []any, escape bool) error {
	for i := range nodes {
		n := &nodes[i]
		switch {
		case n.block != "":
			if err := renderBlock(b, n, scopes, escape); err != nil {
				return err
			}
		case n.path != "":
			v, ok := lookup(scopes, n.path)
			if !ok {
				return &missingError{name: n.path}
			}

			s := format(v)
			if escape && !n.raw {
				s = html.EscapeString(s)
			}

			b.WriteString(s)
		default:
			b.WriteString(n.text)
		}
	}

	return nil
}

func renderBlock(b *strings.Builder, n *node, scopes []any, escape bool) error {
	v, _ := lookup(scopes, n.path)
	switch n.block {
	case "each":
		var items []any
		switch t := v.(type) {
		case []any:
			items = t
		case map[string]any:
			for _, item := range t {
				items = append(items, item)
			}
		}

		if len(items) == 0 {
			return renderTo(b, n.inverse, scopes, escape)
		}

		for i, item := range items {
			scope := map[string]any{pathThis: item, "@index": float64(i), "@first": i == 0, "@last": i == len(items)-1}
			if err := renderTo(b, n.body, append(scopes, item, scope), escape); err != nil {
				return err
			}
		}

		return nil
	case "with":
		if !truthy(v) {
			return renderTo(b, n.inverse, scopes, escape)
		}

		return renderTo(b, n.body, append(scopes, v, map[string]any{pathThis: v}), escape)
	}

	if truthy(v) == (n.block == "if") {
		return renderTo(b, n.body, scopes, escape)
	}

	return renderTo(b, n.inverse, scopes, escape)
}

// lookup resolves a path such as user.name, this, or @index in the innermost
// scope; ../ steps out of it.
func lookup(scopes []any, path string) (any, bool) {
	for strings.HasPrefix(path, "../") && len(scopes) > 1 {
		path, scopes = path[3:], scopes[:len(scopes)-2]
	}

	if len(scopes) > 1 {
		meta, _ := scopes[len(scopes)-1].(map[string]any)
		if v, ok := meta[path]; ok {
			return v, true
		}

		if rest, ok := strings.CutPrefix(path, "this."); ok {
			path = rest
		}

		scopes = scopes[:len(scopes)-1]
	} else if path == pathThis {
		return scopes[0], true
	}

	v := scopes[len(scopes)-1]
	for part := range strings.SplitSeq(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}

		if v, ok = m[part]; !ok {
			return nil, false
		}
	}

	return v, true
}

// truthy is Handlebars's truth: false, null, "", 0, and [] are false.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	}

	return true
}

func format(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, format(e))
		}

		return strings.Join(parts, ",")
	}

	return "[object Object]"
}
