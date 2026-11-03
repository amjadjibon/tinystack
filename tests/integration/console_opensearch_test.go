package integration_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestConsoleOpenSearchLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	send := func(operation string, input map[string]any, status int) map[string]any {
		t.Helper()
		return consoleResponse(
			t,
			srv,
			consoleLambdaRequest(t, srv, "opensearch", operation, account, region, input),
			status,
		)
	}

	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return send(operation, input, http.StatusOK)["output"].(map[string]any)
	}

	refuse := func(operation string, input map[string]any, status int, code string) {
		t.Helper()
		if out := send(operation, input, status); out["code"] != code {
			t.Fatal(operation, out)
		}
	}

	var capabilities map[string]any
	if internal(t, srv, http.MethodGet, consoleActionsPath, &capabilities) != http.StatusOK ||
		!consolePage(capabilities, "opensearch") || capabilities["opensearch_runtime"] != "records" {
		t.Fatal(capabilities)
	}

	// A domain with every option the page sets, and a tag.
	policy := `{"Version":"2012-10-17","Statement":[]}`
	created := call("CreateDomain", map[string]any{
		"DomainName":    "console-search",
		"EngineVersion": "OpenSearch_2.19",
		"ClusterConfig": map[string]any{
			"InstanceType": "m5.large.search", "InstanceCount": 2, "ZoneAwarenessEnabled": true,
			"DedicatedMasterEnabled": true, "DedicatedMasterType": "m5.large.search", "DedicatedMasterCount": 3,
		},
		"EBSOptions": map[string]any{
			"EBSEnabled": true, "VolumeType": "gp3", "VolumeSize": 20, "Iops": 3000, "Throughput": 125,
		},
		"AccessPolicies":              policy,
		"EncryptionAtRestOptions":     map[string]any{"Enabled": true, "KmsKeyId": "alias/console"},
		"NodeToNodeEncryptionOptions": map[string]any{"Enabled": true},
		"DomainEndpointOptions": map[string]any{
			"EnforceHTTPS": true, "TLSSecurityPolicy": "Policy-Min-TLS-1-2-2019-07",
		},
		"TagList": []any{map[string]any{"Key": "team", "Value": "search"}},
	})["DomainStatus"].(map[string]any)
	arn := created["ARN"].(string)
	cluster, volume := created["ClusterConfig"].(map[string]any), created["EBSOptions"].(map[string]any)
	endpoint := created["DomainEndpointOptions"].(map[string]any)
	if arn != "arn:aws:es:eu-west-1:000000000000:domain/console-search" || created["Processing"] != false ||
		created["Deleted"] != false || created["EngineVersion"] != "OpenSearch_2.19" ||
		!strings.HasPrefix(created["Endpoint"].(string), "search-console-search-") ||
		!strings.HasSuffix(created["Endpoint"].(string), ".eu-west-1.es.amazonaws.com") ||
		cluster["InstanceCount"] != float64(2) || cluster["DedicatedMasterType"] != "m5.large.search" ||
		cluster["DedicatedMasterCount"] != float64(3) || cluster["ZoneAwarenessEnabled"] != true ||
		volume["VolumeType"] != "gp3" || volume["Iops"] != float64(3000) || volume["Throughput"] != float64(125) ||
		created["AccessPolicies"] != policy ||
		created["EncryptionAtRestOptions"].(map[string]any)["KmsKeyId"] != "alias/console" ||
		created["NodeToNodeEncryptionOptions"].(map[string]any)["Enabled"] != true ||
		endpoint["EnforceHTTPS"] != true || endpoint["TLSSecurityPolicy"] != "Policy-Min-TLS-1-2-2019-07" {
		t.Fatal(created)
	}

	// A domain from the defaults, then an Elasticsearch one for the engine filter.
	defaults := call("CreateDomain", map[string]any{"DomainName": "console-plain"})["DomainStatus"].(map[string]any)
	plain, plainVolume := defaults["ClusterConfig"].(map[string]any), defaults["EBSOptions"].(map[string]any)
	if defaults["EngineVersion"] != "OpenSearch_2.11" || plain["InstanceType"] != "m5.large.search" ||
		plain["InstanceCount"] != float64(1) || plain["DedicatedMasterEnabled"] != false ||
		plainVolume["VolumeType"] != "gp2" || plainVolume["VolumeSize"] != float64(10) ||
		defaults["DomainEndpointOptions"].(map[string]any)["TLSSecurityPolicy"] != "Policy-Min-TLS-1-0-2019-07" {
		t.Fatal(defaults)
	}

	call("CreateDomain", map[string]any{"DomainName": "console-legacy", "EngineVersion": "Elasticsearch_7.10"})

	refuse("CreateDomain", map[string]any{"DomainName": "console-search"}, http.StatusConflict,
		"ResourceAlreadyExistsException")
	refuse("CreateDomain", map[string]any{"DomainName": "Bad_Name"}, http.StatusBadRequest, "ValidationException")
	refuse("CreateDomain", map[string]any{"DomainName": "console-odd", "ClusterConfig": map[string]any{
		"InstanceCount": 3, "ZoneAwarenessEnabled": true,
	}}, http.StatusBadRequest, "ValidationException")
	refuse("CreateDomain", map[string]any{"DomainName": "console-policy", "AccessPolicies": "not json"},
		http.StatusBadRequest, "ValidationException")

	names := func(engine string) []string {
		t.Helper()
		input := map[string]any{}
		if engine != "" {
			input["EngineType"] = engine
		}

		var out []string
		for _, info := range call("ListDomainNames", input)["DomainNames"].([]any) {
			out = append(out, info.(map[string]any)["DomainName"].(string))
		}

		return out
	}

	if all := names(""); strings.Join(all, ",") != "console-legacy,console-plain,console-search" {
		t.Fatal(all)
	}

	if legacy := names("Elasticsearch"); strings.Join(legacy, ",") != "console-legacy" {
		t.Fatal(legacy)
	}

	// The page describes domains five at a time, which is the most the service takes.
	batch := call("DescribeDomains", map[string]any{"DomainNames": []any{
		"console-search", "console-plain", "console-missing",
	}})["DomainStatusList"].([]any)
	if len(batch) != 2 {
		t.Fatal(batch)
	}

	refuse("DescribeDomains", map[string]any{"DomainNames": []any{"a-1", "a-2", "a-3", "a-4", "a-5", "a-6"}},
		http.StatusBadRequest, "ValidationException")

	// Every option reports its state, which is what the page shows as pending changes, and when the domain was made.
	config := call("DescribeDomainConfig", map[string]any{"DomainName": "console-search"})["DomainConfig"].(map[string]any)
	for _, option := range []string{
		"EngineVersion", "ClusterConfig", "EBSOptions", "AccessPolicies", "EncryptionAtRestOptions",
		"NodeToNodeEncryptionOptions", "DomainEndpointOptions",
	} {
		status := config[option].(map[string]any)["Status"].(map[string]any)
		if status["State"] != "Active" || status["PendingDeletion"] != false || status["CreationDate"].(float64) <= 0 ||
			status["UpdateVersion"] != float64(1) {
			t.Fatal(option, config[option])
		}
	}

	// An update changes the members it names and keeps the others.
	updated := call("UpdateDomainConfig", map[string]any{
		"DomainName":    "console-search",
		"ClusterConfig": map[string]any{"InstanceCount": 4},
		"EBSOptions":    map[string]any{"EBSEnabled": true, "VolumeType": "gp3", "VolumeSize": 40},
	})["DomainConfig"].(map[string]any)
	if updated["ClusterConfig"].(map[string]any)["Status"].(map[string]any)["UpdateVersion"] != float64(2) {
		t.Fatal(updated)
	}

	describe := func(name string) map[string]any {
		t.Helper()
		return call("DescribeDomain", map[string]any{"DomainName": name})["DomainStatus"].(map[string]any)
	}

	settled := func() {
		t.Helper()
		waitFor(
			t,
			"console domain change to finish",
			func() bool { return describe("console-search")["Processing"] == false },
		)
	}

	changed := describe("console-search")
	cluster, volume = changed["ClusterConfig"].(map[string]any), changed["EBSOptions"].(map[string]any)
	if cluster["InstanceCount"] != float64(4) || cluster["InstanceType"] != "m5.large.search" ||
		volume["VolumeSize"] != float64(40) || volume["Iops"] != float64(3000) || changed["AccessPolicies"] != policy {
		t.Fatal(changed)
	}

	settled()

	// Turning dedicated masters off drops their type and count.
	call("UpdateDomainConfig", map[string]any{
		"DomainName": "console-search", "ClusterConfig": map[string]any{"DedicatedMasterEnabled": false},
	})

	cluster = describe("console-search")["ClusterConfig"].(map[string]any)
	if cluster["DedicatedMasterEnabled"] != false || cluster["DedicatedMasterType"] != nil ||
		cluster["DedicatedMasterCount"] != nil {
		t.Fatal(cluster)
	}

	settled()

	// An empty policy removes the access policy, and the page edits the policy and endpoint options.
	call("UpdateDomainConfig", map[string]any{
		"DomainName":     "console-search",
		"AccessPolicies": "",
		"DomainEndpointOptions": map[string]any{
			"EnforceHTTPS":      false,
			"TLSSecurityPolicy": "Policy-Min-TLS-1-2-PFS-2023-10",
		},
	})

	changed = describe("console-search")
	endpoint = changed["DomainEndpointOptions"].(map[string]any)
	if changed["AccessPolicies"] != "" || endpoint["EnforceHTTPS"] != false ||
		endpoint["TLSSecurityPolicy"] != "Policy-Min-TLS-1-2-PFS-2023-10" {
		t.Fatal(changed)
	}

	settled()

	// Settings that cannot be undone or are inconsistent are refused.
	refuse("UpdateDomainConfig", map[string]any{
		"DomainName": "console-search", "EncryptionAtRestOptions": map[string]any{"Enabled": false},
	}, http.StatusBadRequest, "ValidationException")
	refuse("UpdateDomainConfig", map[string]any{
		"DomainName": "console-search", "NodeToNodeEncryptionOptions": map[string]any{"Enabled": false},
	}, http.StatusBadRequest, "ValidationException")
	refuse("UpdateDomainConfig", map[string]any{
		"DomainName": "console-search", "ClusterConfig": map[string]any{"InstanceCount": 3},
	}, http.StatusBadRequest, "ValidationException")
	refuse("UpdateDomainConfig", map[string]any{
		"DomainName": "console-plain", "AccessPolicies": "not json",
	}, http.StatusBadRequest, "ValidationException")
	refuse("UpdateDomainConfig", map[string]any{
		"DomainName": "console-missing", "ClusterConfig": map[string]any{"InstanceCount": 2},
	}, http.StatusConflict, "ResourceNotFoundException")

	// Tags are read and changed by ARN.
	tags := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, tag := range call("ListTags", map[string]any{"ARN": arn})["TagList"].([]any) {
			out[tag.(map[string]any)["Key"].(string)] = tag.(map[string]any)["Value"].(string)
		}

		return out
	}

	if got := tags(); len(got) != 1 || got["team"] != "search" {
		t.Fatal(got)
	}

	call("AddTags", map[string]any{"ARN": arn, "TagList": []any{
		map[string]any{"Key": "env", "Value": "dev"}, map[string]any{"Key": "team", "Value": "platform"},
	}})
	if got := tags(); len(got) != 2 || got["env"] != "dev" || got["team"] != "platform" {
		t.Fatal(got)
	}

	call("RemoveTags", map[string]any{"ARN": arn, "TagKeys": []any{"team"}})
	if got := tags(); len(got) != 1 || got["env"] != "dev" {
		t.Fatal(got)
	}

	refuse("ListTags", map[string]any{"ARN": "arn:aws:es:eu-west-1:000000000000:domain/console-missing"},
		http.StatusConflict, "ResourceNotFoundException")

	// Deleting reports the domain as deleted, and it is gone from every read.
	deleted := call("DeleteDomain", map[string]any{"DomainName": "console-plain"})["DomainStatus"].(map[string]any)
	if deleted["Deleted"] != true || deleted["Processing"] != false {
		t.Fatal(deleted)
	}

	refuse("DescribeDomain", map[string]any{"DomainName": "console-plain"}, http.StatusConflict,
		"ResourceNotFoundException")
	refuse("DeleteDomain", map[string]any{"DomainName": "console-plain"}, http.StatusConflict,
		"ResourceNotFoundException")
	if left := names(""); strings.Join(left, ",") != "console-legacy,console-search" {
		t.Fatal(left)
	}

	// The console can discover versions, check compatibility, and inspect checks and upgrades.
	if versions, _ := call("ListVersions", map[string]any{})["Versions"].([]any); len(versions) == 0 {
		t.Fatal("missing version catalogue")
	}
	compatible := call("GetCompatibleVersions", map[string]any{"DomainName": "console-search"})
	if entries, _ := compatible["CompatibleVersions"].([]any); len(entries) != 1 {
		t.Fatal(compatible)
	}
	for _, checkOnly := range []bool{true, false} {
		call(
			"UpgradeDomain",
			map[string]any{
				"DomainName":       "console-search",
				"TargetVersion":    "OpenSearch_3.1",
				"PerformCheckOnly": checkOnly,
			},
		)
		settled()
		status := call("GetUpgradeStatus", map[string]any{"DomainName": "console-search"})
		if status["StepStatus"] != "SUCCEEDED" {
			t.Fatal(status)
		}
	}
	if describe("console-search")["EngineVersion"] != "OpenSearch_3.1" {
		t.Fatal("upgrade did not change the version")
	}
	history := call("GetUpgradeHistory", map[string]any{"DomainName": "console-search"})
	if entries, _ := history["UpgradeHistories"].([]any); len(entries) != 2 {
		t.Fatal(history)
	}
}
