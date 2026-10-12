package main

import (
	"bytes"
	"testing"
)

func TestJSONDocumentShape(t *testing.T) {
	model := `{"shapes":{
 "fixture#Svc":{"type":"service","traits":{"aws.protocols#awsJson1_1":{},"aws.auth#sigv4":{"name":"fix"}}},
 "fixture#Update":{"type":"operation","input":{"target":"fixture#In"},"output":{"target":"smithy.api#Unit"}},
 "fixture#In":{"type":"structure","members":{"Value":{"target":"fixture#Value"}}},
 "fixture#Value":{"type":"document"}
 }}`
	source, err := generate("fixture", []byte(model), serviceProfile{Operations: []string{"Update"}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(source, []byte("*any")) {
		t.Fatalf("document did not retain JSON values: %s", source)
	}
}
