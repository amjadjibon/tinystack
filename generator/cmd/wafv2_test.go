package main

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

// Verify this live profile against its pinned upstream model. This covers the
// selected input tree, modeled error casing and generated JSON target bindings,
// rather than maintaining a second hand-authored WAF API model.
func TestWAFPinnedProfileProtocol(t *testing.T) {
	raw, err := os.ReadFile("../model/wafv2.json")
	if err != nil {
		t.Fatal(err)
	}
	var m model
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	profileRaw, err := os.ReadFile("../model/profile.json")
	if err != nil {
		t.Fatal(err)
	}
	var profiles map[string]serviceProfile
	if err = json.Unmarshal(profileRaw, &profiles); err != nil {
		t.Fatal(err)
	}
	p := profiles["wafv2"]
	g := generator{model: m, profile: p}
	ns := "com.amazonaws.wafv2#"
	for _, operation := range p.Operations {
		upstream := m.Shapes[ns+operation]
		if upstream.Type != "operation" {
			t.Fatalf("operation not upstream: %s", operation)
		}
		for _, target := range []string{upstream.Input.Target, upstream.Output.Target} {
			short := strings.TrimPrefix(target, ns)
			selected, ok := p.Shapes[short]
			if !ok {
				t.Fatalf("missing explicit profile for %s", short)
			}
			for member, reference := range m.Shapes[target].Members {
				if _, required := reference.Traits["smithy.api#required"]; required &&
					!slices.Contains(selected, member) {
					t.Fatalf("%s omits required member %s", short, member)
				}
			}
		}
		binding, e := compileBinding(m, ns+operation, catalog.AWSJSON11, "AWSWAF_20190729", "", g.fields)
		if e != nil {
			t.Fatal(e)
		}
		// The target's operation component must track the selected upstream symbol.
		if !strings.HasSuffix(binding.Target, "."+operation) || binding.ContentType != "application/x-amz-json-1.1" ||
			binding.Method != "POST" ||
			binding.URI != "/" {
			t.Fatalf("%s binding %+v", operation, binding)
		}
	}
	for _, name := range []string{
		"Rule", "Statement", "RuleAction", "IPSetReferenceStatement", "RuleGroupReferenceStatement", "VisibilityConfig",
	} {
		selected := p.Shapes[name]
		for _, member := range selected {
			if _, ok := m.Shapes[ns+name].Members[member]; !ok {
				t.Fatalf("invented %s.%s", name, member)
			}
		}
		for member, r := range m.Shapes[ns+name].Members {
			if _, ok := r.Traits["smithy.api#required"]; ok && !slices.Contains(selected, member) {
				t.Fatalf("required %s.%s omitted", name, member)
			}
		}
	}
	errors, err := g.errorBindings(p.Operations, ns, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"WAFInvalidParameterException", "WAFNonexistentItemException", "WAFOptimisticLockException",
		"WAFAssociatedItemException", "WAFLimitsExceededException",
	} {
		binding, ok := errors[name]
		if !ok {
			t.Fatalf("missing modeled error %s", name)
		}
		upstream := m.Shapes[ns+name]
		if _, ok := upstream.Members[binding.MessageMember]; !ok {
			t.Fatalf("%s message casing %q is not modeled", name, binding.MessageMember)
		}
	}
	if _, err = generate("wafv2", raw, p); err != nil {
		t.Fatal(err)
	}
}
