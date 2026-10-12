package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

func TestTransferPinnedIdentityProtocolBindingsAndProvenance(t *testing.T) {
	raw, e := os.ReadFile("../model/transfer.json")
	if e != nil {
		t.Fatal(e)
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != "dacf9d6638d8a4f4621ebc438aa8dab7589e757b16037ea8acf50cb292e1db83" {
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
	var profiles map[string]serviceProfile
	if e = json.Unmarshal(raw, &profiles); e != nil {
		t.Fatal(e)
	}
	g := generator{model: m, profile: profiles["transfer"]}
	if len(g.profile.Operations) != 15 {
		t.Fatal("surface")
	}
	for _, op := range g.profile.Operations {
		b, e := compileBinding(m, "com.amazonaws.transfer#"+op, catalog.AWSJSON11, "TransferService", "", g.fields)
		if e != nil {
			t.Fatal(e)
		}
		if b.Method != "POST" || b.URI != "/" || b.Target != "TransferService."+op || b.StatusCode != 200 ||
			b.ContentType != "application/x-amz-json-1.1" ||
			len(b.Input) != 0 {
			t.Fatalf("%s: %+v", op, b)
		}
	}
	for _, name := range []string{
		"ResourceExistsException",
		"ResourceNotFoundException",
		"InvalidNextTokenException",
		"InvalidRequestException",
	} {
		shape := m.Shapes["com.amazonaws.transfer#"+name]
		var status int
		json.Unmarshal(shape.Traits["smithy.api#httpError"], &status)
		expected := map[string]int{
			"ResourceExistsException":   409,
			"ResourceNotFoundException": 404,
			"InvalidNextTokenException": 400,
			"InvalidRequestException":   400,
		}
		if status != expected[name] {
			t.Fatal(name, status)
		}
		if _, ok := shape.Members["Message"]; !ok {
			t.Fatal("modeled Message")
		}
	}
}
