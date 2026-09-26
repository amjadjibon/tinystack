package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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
  "fixture#Missing": {"type": "structure", "members": {"Message": {"target": "smithy.api#String"}},
    "traits": {"smithy.api#error": "client",
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
		`"Missing": catalog.ErrorBinding{QueryCode: "Fixture.Missing", MessageMember: "Message", StatusCode: 404}`,
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
  "fixture#In": {"type": "structure", "members": {"Ids": {"target": "fixture#IdList"},
    "At": {"target": "smithy.api#Timestamp"}}},
  "fixture#IdList": {"type": "list", "member": {"target": "smithy.api#String"}},
  "fixture#Missing": {"type": "structure", "traits": {"smithy.api#error": "client"}},
  "smithy.api#String": {"type": "string"},
  "smithy.api#Timestamp": {"type": "timestamp"}
}}`
	profile := serviceProfile{Operations: []string{"Put"}}
	source, err := generate("fixture", []byte(model), profile)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`query:"Ids"`,
		"At  *time.Time `json:\"At,omitempty\" query:\"At\"`",
		`TargetPrefix: "", QueryCompatible: false, XMLNamespace: "urn:fixture", Protocols: []string{"awsQuery"}`,
		`"Missing": catalog.ErrorBinding{QueryCode: "Missing", MessageMember: "", StatusCode: 400}`,
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

func TestEC2QueryFixture(t *testing.T) {
	model := `{"shapes": {
  "fixture#Svc": {"type": "service", "version": "2016-11-15", "traits": {
    "aws.protocols#ec2Query": {}, "aws.auth#sigv4": {"name": "ec2"},
    "smithy.api#xmlNamespace": {"uri": "urn:ec2"}}},
  "fixture#Describe": {"type": "operation", "input": {"target": "fixture#In"}, "output": {"target": "fixture#Out"}},
  "fixture#In": {"type": "structure", "members": {
    "InstanceIds": {"target": "fixture#IdList", "traits": {"smithy.api#xmlName": "InstanceId"}},
    "DryRun": {"target": "smithy.api#Boolean",
      "traits": {"aws.protocols#ec2QueryName": "DryRun", "smithy.api#xmlName": "dryRun"}},
    "Plain": {"target": "smithy.api#String"}}},
  "fixture#Out": {"type": "structure", "members": {
    "Items": {"target": "fixture#ItemList",
      "traits": {"aws.protocols#ec2QueryName": "ItemSet", "smithy.api#xmlName": "itemSet"}},
    "At": {"target": "smithy.api#Timestamp"}}},
  "fixture#IdList": {"type": "list", "member": {"target": "smithy.api#String",
    "traits": {"smithy.api#xmlName": "InstanceId"}}},
  "fixture#ItemList": {"type": "list", "member": {"target": "smithy.api#String",
    "traits": {"smithy.api#xmlName": "item"}}},
  "smithy.api#String": {"type": "string"},
  "smithy.api#Boolean": {"type": "boolean"},
  "smithy.api#Timestamp": {"type": "timestamp"}
}}`
	source, err := generate("fixture", []byte(model), serviceProfile{Operations: []string{"Describe"}})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`ec2:"InstanceId,InstanceId,member=InstanceId"`,
		`ec2:"DryRun,dryRun"`,
		`ec2:"Plain,Plain"`,
		`ec2:"ItemSet,itemSet,member=item"`,
		"*time.Time",
		`XMLNamespace: "urn:ec2", Protocols: []string{"ec2Query"}`,
		`Target: "Describe", Version: "2016-11-15", ContentType: "application/x-www-form-urlencoded"`,
	} {
		if !strings.Contains(string(source), want) {
			t.Fatalf("generated source lacks %s:\n%s", want, source)
		}
	}

	profile := serviceProfile{Operations: []string{"Describe"}, Query: &queryOverlay{XMLNamespace: "urn:other"}}
	if _, err = generate("fixture", []byte(model), profile); err == nil {
		t.Fatal("query overlay accepted for an ec2Query model")
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

func TestRESTJSONFixture(t *testing.T) {
	model := `{"shapes": {
  "fixture#Svc": {"type": "service", "version": "2020-01-01", "traits": {
    "aws.protocols#restJson1": {}, "aws.auth#sigv4": {"name": "fix"}}},
  "fixture#Run": {"type": "operation", "input": {"target": "fixture#In"}, "output": {"target": "fixture#Out"},
    "errors": [{"target": "fixture#Missing"}],
    "traits": {"smithy.api#http": {"method": "POST", "uri": "/runs/{Name}"}}},
  "fixture#In": {"type": "structure", "members": {
    "Name": {"target": "smithy.api#String", "traits": {"smithy.api#httpLabel": {}, "smithy.api#required": {}}},
    "Mode": {"target": "smithy.api#String", "traits": {"smithy.api#httpHeader": "X-Mode"}},
    "Payload": {"target": "smithy.api#Blob", "traits": {"smithy.api#httpPayload": {}}}}},
  "fixture#Out": {"type": "structure", "members": {
    "Status": {"target": "smithy.api#Integer", "traits": {"smithy.api#httpResponseCode": {}}},
    "At": {"target": "smithy.api#Timestamp"}}},
  "fixture#Missing": {"type": "structure", "traits": {"smithy.api#error": "client", "smithy.api#httpError": 404},
    "members": {"Message": {"target": "smithy.api#String"}}},
  "smithy.api#String": {"type": "string"},
  "smithy.api#Blob": {"type": "blob"},
  "smithy.api#Integer": {"type": "integer"},
  "smithy.api#Timestamp": {"type": "timestamp"}
}}`
	profile := serviceProfile{Operations: []string{"Run"}, Shapes: map[string][]string{"In": {"Name", "Payload"}}}
	source, err := generate("fixture", []byte(model), profile)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`Name *string ` + "`" + `json:"-"` + "`",
		`At *float64 ` + "`" + `json:"At,omitempty"` + "`",
		`Protocols: []string{"restJson1"}`,
		// Smithy's default success code applies when the http trait has none.
		`StatusCode: 200`,
		`"Status": catalog.MemberBinding{Location: "status", Name: "Status"}`,
		`"Mode": catalog.MemberBinding{Location: "unsupported:header", Name: "X-Mode"}`,
		`"Missing": catalog.ErrorBinding{QueryCode: "", MessageMember: "Message", StatusCode: 404}`,
	} {
		// Compare without gofmt's column alignment.
		if !strings.Contains(strings.Join(strings.Fields(string(source)), " "), want) {
			t.Fatalf("generated source lacks %s:\n%s", want, source)
		}
	}

	if strings.Contains(string(source), "encoding/xml") {
		t.Fatalf("REST JSON service has XML tags:\n%s", source)
	}
}

func TestJSON11Fixture(t *testing.T) {
	model := `{"shapes": {
  "fixture#Svc": {"type": "service", "version": "2020-01-01", "traits": {
    "aws.protocols#awsJson1_1": {}, "aws.auth#sigv4": {"name": "fix"}}},
  "fixture#Put": {"type": "operation", "input": {"target": "fixture#In"}, "output": {"target": "smithy.api#Unit"}},
  "fixture#In": {"type": "structure", "members": {"Name": {"target": "smithy.api#String"}}},
  "smithy.api#String": {"type": "string"}
}}`
	source, err := generate("fixture", []byte(model), serviceProfile{Operations: []string{"Put"}})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`TargetPrefix: "Svc"`,
		`Protocols: []string{"awsJson1_1"}`,
		`Target: "Svc.Put"`,
		`ContentType: "application/x-amz-json-1.1"`,
	} {
		if !strings.Contains(strings.Join(strings.Fields(string(source)), " "), want) {
			t.Fatalf("generated source lacks %s:\n%s", want, source)
		}
	}

	// AWS Query compatibility is defined only for AWS JSON 1.0.
	compatible := strings.Replace(model, `"aws.protocols#awsJson1_1": {}`,
		`"aws.protocols#awsJson1_1": {}, "aws.protocols#awsQueryCompatible": {}`, 1)
	if _, err = generate("fixture", []byte(compatible), serviceProfile{Operations: []string{"Put"}}); err == nil {
		t.Fatal("query-compatible AWS JSON 1.1 model generated")
	}
}

func TestSensitiveOperations(t *testing.T) {
	model := `{"shapes": {
  "fixture#Svc": {"type": "service", "version": "2020-01-01", "traits": {
    "aws.protocols#awsJson1_1": {}, "aws.auth#sigv4": {"name": "fix"}}},
  "fixture#Put": {"type": "operation", "input": {"target": "fixture#PutIn"}, "output": {"target": "smithy.api#Unit"}},
  "fixture#Get": {"type": "operation", "input": {"target": "fixture#GetIn"}, "output": {"target": "fixture#GetOut"}},
  "fixture#List": {"type": "operation", "input": {"target": "fixture#GetIn"}, "output": {"target": "fixture#ListOut"}},
  "fixture#PutIn": {"type": "structure", "members": {"Name": {"target": "smithy.api#String"}}},
  "fixture#GetIn": {"type": "structure", "members": {"Name": {"target": "smithy.api#String"}}},
  "fixture#GetOut": {"type": "structure", "members": {"Values": {"target": "fixture#Values"}}},
  "fixture#ListOut": {"type": "structure", "members": {"Name": {"target": "smithy.api#String"},
    "Value": {"target": "fixture#Secret"}}},
  "fixture#Values": {"type": "map", "key": {"target": "smithy.api#String"}, "value": {"target": "fixture#Secret"}},
  "fixture#Secret": {"type": "string", "traits": {"smithy.api#sensitive": {}}},
  "smithy.api#String": {"type": "string"}
}}`
	// List's sensitive Value is not generated, so List carries no secret.
	profile := serviceProfile{
		Operations: []string{"Put", "Get", "List"},
		Shapes:     map[string][]string{"ListOut": {"Name"}},
	}
	source, err := generate("fixture", []byte(model), profile)
	if err != nil {
		t.Fatal(err)
	}

	flat := strings.Join(strings.Fields(string(source)), " ")
	for op, want := range map[string]bool{"Put": false, "Get": true, "List": false} {
		if field := fmt.Sprintf(`Name: %q, UnitOutput: %t, Sensitive: %t`, op, op == "Put", want); !strings.Contains(
			flat, field) {
			t.Errorf("generated source lacks %s", field)
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
