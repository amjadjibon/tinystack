package main

import (
	"strings"
	"testing"
)

func TestDocumentPrelude(t *testing.T) {
	model := `{"shapes":{
 "fixture#Service":{"type":"service","version":"1","traits":{
 "aws.protocols#awsJson1_0":{},"aws.auth#sigv4":{"name":"fixture"}}},
 "fixture#Get":{"type":"operation","input":{"target":"fixture#Input"},"output":{"target":"fixture#Output"}},
 "fixture#Input":{"type":"structure"},
 "fixture#Output":{"type":"structure","members":{"policy":{"target":"smithy.api#Document"}}}
 }}`
	source, err := generate("fixture", []byte(model), serviceProfile{Operations: []string{"Get"}})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(source), "Policy *any") {
		t.Fatalf("document prelude not generated: %s", source)
	}
}
