package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

func TestLocationPinnedIdentityProtocolBindingsAndProvenance(t *testing.T) {
	raw, e := os.ReadFile("../model/location.json")
	if e != nil {
		t.Fatal(e)
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != "b6afe8e89a91ebf6cedb12595cadaa8168aa08483160017904d8b44d3daa8d89" {
		t.Fatal("model hash")
	}
	var m model
	if e = json.Unmarshal(raw, &m); e != nil {
		t.Fatal(e)
	}
	raw, e = os.ReadFile("../model/profile.json")
	if e != nil {
		t.Fatal(e)
	}
	var p map[string]serviceProfile
	if e = json.Unmarshal(raw, &p); e != nil {
		t.Fatal(e)
	}
	service := m.Shapes["com.amazonaws.location#LocationService"]
	var identity struct{ SDKID, ARNNamespace string }
	if e = json.Unmarshal(service.Traits["aws.api#service"], &identity); e != nil {
		t.Fatal(e)
	}
	var signing struct{ Name string }
	json.Unmarshal(service.Traits["aws.auth#sigv4"], &signing)
	if identity.SDKID != "Location" || identity.ARNNamespace != "geo" || signing.Name != "geo" {
		t.Fatal(identity, signing)
	}
	expected := map[string]struct{ verb, path string }{
		"CreateTracker":       {"POST", "/tracking/v0/trackers"},
		"DescribeTracker":     {"GET", "/tracking/v0/trackers/{TrackerName}"},
		"ListTrackers":        {"POST", "/tracking/v0/list-trackers"},
		"UpdateTracker":       {"PATCH", "/tracking/v0/trackers/{TrackerName}"},
		"DeleteTracker":       {"DELETE", "/tracking/v0/trackers/{TrackerName}"},
		"ListTagsForResource": {"GET", "/tags/{ResourceArn}"},
		"TagResource":         {"POST", "/tags/{ResourceArn}"},
		"UntagResource":       {"DELETE", "/tags/{ResourceArn}"},
	}
	g := generator{model: m, profile: p["location"]}
	if len(g.profile.Operations) != len(expected) {
		t.Fatal("surface")
	}
	for _, op := range g.profile.Operations {
		b, e := compileBinding(m, "com.amazonaws.location#"+op, catalog.RESTJSON, "", "", g.fields)
		if e != nil {
			t.Fatal(e)
		}
		want := expected[op]
		if b.Method != want.verb || b.URI != want.path || b.StatusCode != 200 || b.ContentType != "application/json" {
			t.Fatal(op, b)
		}
		if op == "UntagResource" && (b.Input["TagKeys"].Name != "tagKeys" || b.Input["TagKeys"].Location != "query") {
			t.Fatal("query identity")
		}
		if op == "UpdateTracker" &&
			(b.Input["TrackerName"].Location != "label" || b.Input["Description"].Location != "document") {
			t.Fatal("update identity")
		}
	}
	for name, want := range map[string]int{
		"ConflictException":         409,
		"ResourceNotFoundException": 404,
		"ValidationException":       400,
	} {
		shape := m.Shapes["com.amazonaws.location#"+name]
		var status int
		json.Unmarshal(shape.Traits["smithy.api#httpError"], &status)
		var messageName string
		json.Unmarshal(shape.Members["Message"].Traits["smithy.api#jsonName"], &messageName)
		if status != want || messageName != "message" {
			t.Fatal(name, status, messageName)
		}
	}
}
