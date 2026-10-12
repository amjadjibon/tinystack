package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

func TestS3TablesPinnedIdentityBindingsAndMemberTimestamps(t *testing.T) {
	raw, e := os.ReadFile("../model/s3tables.json")
	if e != nil {
		t.Fatal(e)
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != "8b92d3d062c80f66dd9b2c600ff15230656e715cea567ddbfc750e446b20927d" {
		t.Fatal("pinned provenance")
	}
	var m model
	if e = json.Unmarshal(raw, &m); e != nil {
		t.Fatal(e)
	}
	pRaw, e := os.ReadFile("../model/profile.json")
	if e != nil {
		t.Fatal(e)
	}
	var p map[string]serviceProfile
	if e = json.Unmarshal(pRaw, &p); e != nil {
		t.Fatal(e)
	}
	service := m.Shapes["com.amazonaws.s3tables#S3TableBuckets"]
	var identity struct{ SDKID, ARNNamespace string }
	json.Unmarshal(service.Traits["aws.api#service"], &identity)
	var signing struct{ Name string }
	json.Unmarshal(service.Traits["aws.auth#sigv4"], &signing)
	if identity.SDKID != "S3Tables" || identity.ARNNamespace != "s3tables" || signing.Name != "s3tables" {
		t.Fatal(identity, signing)
	}
	g := generator{model: m, profile: p["s3tables"]}
	expected := map[string]struct {
		method, path string
		status       int
	}{
		"CreateTableBucket": {
			"PUT",
			"/buckets",
			200,
		},
		"GetTableBucket":    {"GET", "/buckets/{tableBucketARN}", 200},
		"ListTableBuckets":  {"GET", "/buckets", 200},
		"DeleteTableBucket": {"DELETE", "/buckets/{tableBucketARN}", 204},
		"CreateNamespace": {
			"PUT",
			"/namespaces/{tableBucketARN}",
			200,
		},
		"GetNamespace":    {"GET", "/namespaces/{tableBucketARN}/{namespace}", 200},
		"ListNamespaces":  {"GET", "/namespaces/{tableBucketARN}", 200},
		"DeleteNamespace": {"DELETE", "/namespaces/{tableBucketARN}/{namespace}", 204},
		"CreateTable": {
			"PUT",
			"/tables/{tableBucketARN}/{namespace}",
			200,
		},
		"GetTable":    {"GET", "/get-table", 200},
		"ListTables":  {"GET", "/tables/{tableBucketARN}", 200},
		"DeleteTable": {"DELETE", "/tables/{tableBucketARN}/{namespace}/{name}", 204},
		"RenameTable": {"PUT", "/tables/{tableBucketARN}/{namespace}/{name}/rename", 204},
		"TagResource": {
			"POST",
			"/tag/{resourceArn}",
			200,
		},
		"ListTagsForResource": {"GET", "/tag/{resourceArn}", 200},
		"UntagResource":       {"DELETE", "/tag/{resourceArn}", 204},
	}
	if len(g.profile.Operations) != len(expected) {
		t.Fatal("surface")
	}
	for _, op := range g.profile.Operations {
		b, bindingErr := compileBinding(m, "com.amazonaws.s3tables#"+op, catalog.RESTJSON, "", "", g.fields)
		if bindingErr != nil {
			t.Fatal(bindingErr)
		}
		want := expected[op]
		if b.Method != want.method || b.URI != want.path || b.StatusCode != want.status ||
			b.ContentType != "application/json" {
			t.Fatal(op, b)
		}
	}
	source, e := generate("s3tables", raw, p["s3tables"])
	if e != nil {
		t.Fatal(e)
	}
	normalized := strings.Join(strings.Fields(string(source)), " ")
	for _, member := range []string{"CreatedAt", "ModifiedAt"} {
		if !strings.Contains(normalized, member+" *time.Time") {
			t.Fatal("member timestamp wire override lost")
		}
	}
	for _, name := range []string{
		"GetTableMetadataLocation",
		"UpdateTableMetadataLocation",
		"MetadataLocation",
		"WarehouseLocation",
		"IcebergSchema",
	} {
		if strings.Contains(string(source), name) {
			t.Fatal("unimplemented member or operation selected", name)
		}
	}
	for name, status := range map[string]int{
		"BadRequestException": 400,
		"NotFoundException":   404,
		"ConflictException":   409,
	} {
		v := m.Shapes["com.amazonaws.s3tables#"+name]
		var got int
		json.Unmarshal(v.Traits["smithy.api#httpError"], &got)
		if got != status {
			t.Fatal(name, got)
		}
	}
}
