package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

func TestCodeBuildPinnedJSONBindingsAndSelection(t *testing.T) {
	raw, e := os.ReadFile("../model/codebuild.json")
	if e != nil {
		t.Fatal(e)
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
	g := generator{model: m, profile: p["codebuild"]}
	for _, op := range p["codebuild"].Operations {
		b, e := compileBinding(m, "com.amazonaws.codebuild#"+op, catalog.AWSJSON11, "CodeBuild_20161006", "", g.fields)
		if e != nil {
			t.Fatal(e)
		}
		if b.Method != "POST" || b.URI != "/" || b.StatusCode != 200 || b.Target != "CodeBuild_20161006."+op ||
			b.ContentType != "application/x-amz-json-1.1" {
			t.Fatalf("%s: %+v", op, b)
		}
	}
	if len(p["codebuild"].Operations) != 5 {
		t.Fatal("unexpected operation surface")
	}
	if len(p["codebuild"].Shapes["ProjectSource"]) != 4 || len(p["codebuild"].Shapes["ProjectEnvironment"]) != 5 {
		t.Fatal("advanced source/environment inputs were selected")
	}
}
