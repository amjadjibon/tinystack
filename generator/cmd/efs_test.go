package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

func TestEFSPinnedRESTBindings(t *testing.T) {
	raw, err := os.ReadFile("../model/efs.json")
	if err != nil {
		t.Fatal(err)
	}
	var m model
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile("../model/profile.json")
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]serviceProfile
	if err = json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	g := generator{model: m, profile: p["efs"]}
	for _, test := range []struct {
		op, method, uri, member, location, name string
		status                                  int
	}{
		{"CreateFileSystem", "POST", "/2015-02-01/file-systems", "Backup", "", "", 201},
		{"UpdateFileSystem", "PUT", "/2015-02-01/file-systems/{FileSystemId}", "FileSystemId", "label", "FileSystemId", 202},
		{"CreateMountTarget", "POST", "/2015-02-01/mount-targets", "Ipv6Address", "", "", 200},
		{"DescribeAccessPoints", "GET", "/2015-02-01/access-points", "NextToken", "query", "NextToken", 200},
		{"UntagResource", "DELETE", "/2015-02-01/resource-tags/{ResourceId}", "TagKeys", "query", "tagKeys", 200},
		{
			"DeleteFileSystem", "DELETE", "/2015-02-01/file-systems/{FileSystemId}",
			"FileSystemId", "label", "FileSystemId", 204,
		},
	} {
		b, err := compileBinding(m, "com.amazonaws.efs#"+test.op, catalog.RESTJSON, "", "", g.fields)
		if err != nil {
			t.Fatal(err)
		}
		member := b.Input[test.member]
		if b.Method != test.method || b.URI != test.uri || b.StatusCode != test.status ||
			member.Location != test.location ||
			member.Name != test.name {
			t.Fatalf("%s binding: %+v", test.op, b)
		}
	}
}
