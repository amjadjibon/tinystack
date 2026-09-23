package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReproducibleGeneration(t *testing.T) {
	root := t.TempDir()
	models := filepath.Join(root, "generator", "model")
	if guardErr := os.MkdirAll(models, 0o755); guardErr != nil {
		t.Fatal(guardErr)
	}

	entries, err := os.ReadDir(filepath.Join("..", "model"))
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		b, readErr := os.ReadFile(filepath.Join("..", "model", entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}

		if guardErr := os.WriteFile(filepath.Join(models, entry.Name()), b, 0o644); guardErr != nil {
			t.Fatal(guardErr)
		}
	}

	generated := func() map[string][]byte {
		t.Helper()
		if guardErr := run(root); guardErr != nil {
			t.Fatal(guardErr)
		}

		files := map[string][]byte{}
		dirs, readErr := os.ReadDir(filepath.Join(root, "internal", "aws", "generated"))
		if readErr != nil {
			t.Fatal(readErr)
		}

		for _, dir := range dirs {
			b, fileErr := os.ReadFile(filepath.Join(root, "internal", "aws", "generated", dir.Name(), "api.go"))
			if fileErr != nil {
				t.Fatal(fileErr)
			}

			files[dir.Name()] = b
		}

		return files
	}

	first, second := generated(), generated()
	if len(first) < 2 {
		t.Fatalf("expected every profiled service to be generated, got %d", len(first))
	}

	for name, source := range first {
		if !bytes.Equal(source, second[name]) {
			t.Fatalf("%s generation is not deterministic", name)
		}

		committed, readErr := os.ReadFile(filepath.Join("..", "..", "internal", "aws", "generated", name, "api.go"))
		if readErr != nil {
			t.Fatal(readErr)
		}

		if !bytes.Equal(source, committed) {
			t.Fatalf("generated %s source is stale; run go generate ./...", name)
		}
	}

	if guardErr := os.WriteFile(filepath.Join(models, "dynamodb.json"), []byte(`{}`), 0o644); guardErr != nil {
		t.Fatal(guardErr)
	}

	if err = run(root); err == nil {
		t.Fatal("modified model passed checksum verification")
	}
}

func TestQueryOverlayFixture(t *testing.T) {
	model := `{"shapes": {
  "fixture#Svc": {"type": "service", "version": "2020-01-01", "traits": {
    "aws.protocols#awsJson1_0": {}, "aws.protocols#awsQueryCompatible": {}, "aws.auth#sigv4": {"name": "fix"}}},
  "fixture#Put": {"type": "operation", "input": {"target": "fixture#In"}, "output": {"target": "smithy.api#Unit"},
    "errors": [{"target": "fixture#Missing"}]},
  "fixture#In": {"type": "structure", "members": {
    "Tags": {"target": "fixture#TagMap", "traits": {"smithy.api#xmlFlattened": {}, "smithy.api#xmlName": "Tag"}},
    "Ids": {"target": "fixture#IdList"}}},
  "fixture#TagMap": {"type": "map", "key": {"target": "smithy.api#String", "traits": {"smithy.api#xmlName": "Key"}},
    "value": {"target": "smithy.api#String"}},
  "fixture#IdList": {"type": "list", "member": {"target": "smithy.api#String", "traits": {"smithy.api#xmlName": "Id"}}},
  "fixture#Missing": {"type": "structure", "traits": {"smithy.api#error": "client",
    "aws.protocols#awsQueryError": {"code": "Fixture.Missing", "httpResponseCode": 404}}},
  "smithy.api#String": {"type": "string"}
}}`
	profile := serviceProfile{Operations: []string{"Put"}, Query: &queryOverlay{XMLNamespace: "urn:fixture"}}
	source, err := generate("fixture", []byte(model), profile)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`query:"Tag,flat,key=Key"`,
		`query:"Ids,member=Id"`,
		"type PutOutput struct{}",
		`"Missing": catalog.ErrorBinding{QueryCode: "Fixture.Missing", StatusCode: 404}`,
		`Target: "Put", Version: "2020-01-01", ContentType: "application/x-www-form-urlencoded"`,
		`XMLNamespace: "urn:fixture"`,
	} {
		if !strings.Contains(string(source), want) {
			t.Fatalf("generated source lacks %s:\n%s", want, source)
		}
	}

	profile.Query = &queryOverlay{}
	if _, err = generate("fixture", []byte(model), profile); err == nil {
		t.Fatal("query overlay without namespace accepted")
	}
}

func TestPureQueryFixture(t *testing.T) {
	model := `{"shapes": {
  "fixture#Svc": {"type": "service", "version": "2020-01-01", "traits": {
    "aws.protocols#awsQuery": {}, "aws.auth#sigv4": {"name": "fix"},
    "smithy.api#xmlNamespace": {"uri": "urn:fixture"}}},
  "fixture#Put": {"type": "operation", "input": {"target": "fixture#In"}, "output": {"target": "smithy.api#Unit"},
    "errors": [{"target": "fixture#Missing"}]},
  "fixture#In": {"type": "structure", "members": {"Ids": {"target": "fixture#IdList"}}},
  "fixture#IdList": {"type": "list", "member": {"target": "smithy.api#String"}},
  "fixture#Missing": {"type": "structure", "traits": {"smithy.api#error": "client"}},
  "smithy.api#String": {"type": "string"}
}}`
	profile := serviceProfile{Operations: []string{"Put"}}
	source, err := generate("fixture", []byte(model), profile)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`query:"Ids"`,
		`TargetPrefix: "", QueryCompatible: false, XMLNamespace: "urn:fixture", Protocols: []string{"awsQuery"}`,
		`"Missing": catalog.ErrorBinding{QueryCode: "Missing", StatusCode: 400}`,
		`Target: "Put", Version: "2020-01-01", ContentType: "application/x-www-form-urlencoded"`,
	} {
		if !strings.Contains(string(source), want) {
			t.Fatalf("generated source lacks %s:\n%s", want, source)
		}
	}

	if strings.Contains(string(source), "awsJson1_0") {
		t.Fatalf("pure awsQuery service has a JSON binding:\n%s", source)
	}

	profile.Query = &queryOverlay{XMLNamespace: "urn:other"}
	if _, err = generate("fixture", []byte(model), profile); err == nil {
		t.Fatal("query overlay accepted for a pure awsQuery model")
	}
}

func TestRESTXMLFixture(t *testing.T) {
	model := `{"shapes": {
  "fixture#Svc": {"type": "service", "version": "2020-01-01", "traits": {
    "aws.protocols#restXml": {"noErrorWrapping": true}, "aws.auth#sigv4": {"name": "fix"},
    "smithy.api#xmlNamespace": {"uri": "urn:fixture"}}},
  "fixture#List": {"type": "operation", "input": {"target": "fixture#In"}, "output": {"target": "fixture#Out"},
    "traits": {"smithy.api#http": {"method": "GET", "uri": "/{Bucket}?list", "code": 200}}},
  "fixture#In": {"type": "structure", "members": {
    "Bucket": {"target": "smithy.api#String", "traits": {"smithy.api#httpLabel": {}, "smithy.api#required": {}}},
    "Prefix": {"target": "smithy.api#String", "traits": {"smithy.api#httpQuery": "prefix"}},
    "Owner": {"target": "smithy.api#String", "traits": {"smithy.api#httpHeader": "x-owner"}}}},
  "fixture#Out": {"type": "structure", "traits": {"smithy.api#xmlName": "Result"}, "members": {
    "Items": {"target": "fixture#ItemList", "traits": {"smithy.api#xmlFlattened": {}}},
    "Names": {"target": "fixture#NameList"},
    "Meta": {"target": "fixture#MetaMap", "traits": {"smithy.api#httpPrefixHeaders": "x-meta-"}}}},
  "fixture#ItemList": {"type": "list", "member": {"target": "fixture#Item"}},
  "fixture#NameList": {"type": "list",
    "member": {"target": "smithy.api#String", "traits": {"smithy.api#xmlName": "Name"}}},
  "fixture#MetaMap": {"type": "map", "key": {"target": "smithy.api#String"}, "value": {"target": "smithy.api#String"}},
  "fixture#Item": {"type": "structure", "members": {"When": {"target": "smithy.api#Timestamp"}}},
  "smithy.api#String": {"type": "string"},
  "smithy.api#Timestamp": {"type": "timestamp"}
}}`
	profile := serviceProfile{Operations: []string{"List"}, Shapes: map[string][]string{"In": {"Bucket", "Prefix"}}}
	source, err := generate("fixture", []byte(model), profile)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`XMLName xml.Name ` + "`" + `json:"-" xml:"urn:fixture Result"` + "`",
		`Items []Item ` + "`" + `json:"Items,omitempty" xml:"Items,omitempty"` + "`",
		`xml:"Names>Name,omitempty"`,
		`When *time.Time`,
		`Protocols: []string{"restXml"}`,
		`"Owner": catalog.MemberBinding{Location: "unsupported:header", Name: "x-owner"}`,
		`"Meta": catalog.MemberBinding{Location: "prefix", Name: "x-meta-"}`,
		`URI: "/{Bucket}?list"`,
	} {
		// Compare without gofmt's column alignment.
		if !strings.Contains(strings.Join(strings.Fields(string(source)), " "), want) {
			t.Fatalf("generated source lacks %s:\n%s", want, source)
		}
	}
}

func TestUnsupportedWireTraitsFail(t *testing.T) {
	for _, trait := range []string{
		"smithy.api#streaming", "smithy.api#httpHeader",
		"smithy.api#jsonName", "smithy.api#timestampFormat",
		"smithy.api#xmlAttribute", "smithy.api#xmlNamespace",
	} {
		t.Run(trait, func(t *testing.T) {
			if guardErr := checkTraits(
				map[string]json.RawMessage{trait: json.RawMessage(`{}`)},
				false,
			); guardErr == nil {
				t.Fatal("unsupported trait accepted")
			}
		})
	}
}
