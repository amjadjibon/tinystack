package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

// The pinned Backup model supplies all routes, query names and REST error
// metadata. In particular omitted query members remain explicit unsupported
// bindings and selection/tag deletion has a Smithy Unit output.
func TestBackupPinnedBindings(t *testing.T) {
	raw, err := os.ReadFile("../model/backup.json")
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
	var profiles map[string]serviceProfile
	if err = json.Unmarshal(raw, &profiles); err != nil {
		t.Fatal(err)
	}
	g := generator{model: m, profile: profiles["backup"]}
	for _, test := range []struct{ operation, method, uri, member, location, name string }{
		{"CreateBackupVault", "PUT", "/backup-vaults/{BackupVaultName}", "BackupVaultName", "label", "BackupVaultName"},
		{"GetBackupPlan", "GET", "/backup/plans/{BackupPlanId}", "VersionId", "query", "versionId"},
		{
			"GetBackupPlan", "GET", "/backup/plans/{BackupPlanId}",
			"MaxScheduledRunsPreview", "unsupported:query", "MaxScheduledRunsPreview",
		},
		{
			"DescribeBackupVault", "GET", "/backup-vaults/{BackupVaultName}",
			"BackupVaultAccountId", "unsupported:query", "backupVaultAccountId",
		},
		{"ListBackupVaults", "GET", "/backup-vaults", "ByShared", "query", "shared"},
		{"ListBackupPlans", "GET", "/backup/plans", "IncludeDeleted", "query", "includeDeleted"},
		{"TagResource", "POST", "/tags/{ResourceArn}", "ResourceArn", "label", "ResourceArn"},
		{"UntagResource", "POST", "/untag/{ResourceArn}", "TagKeyList", "document", "TagKeyList"},
	} {
		binding, err := compileBinding(m, "com.amazonaws.backup#"+test.operation, catalog.RESTJSON, "", "", g.fields)
		if err != nil {
			t.Fatal(err)
		}
		member := binding.Input[test.member]
		if binding.Method != test.method || binding.URI != test.uri || binding.ContentType != "application/json" ||
			binding.StatusCode != 200 ||
			member.Location != test.location ||
			member.Name != test.name {
			t.Fatalf("%s wire binding: %+v", test.operation, binding)
		}
	}
	for _, operation := range []string{"DeleteBackupVault", "DeleteBackupSelection", "TagResource", "UntagResource"} {
		if m.Shapes["com.amazonaws.backup#"+operation].Output.Target != unitShape {
			t.Fatalf("%s lost Unit output", operation)
		}
	}
}
