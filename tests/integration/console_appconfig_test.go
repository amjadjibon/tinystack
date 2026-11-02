package integration_test

import (
	"encoding/base64"
	"net/http"
	"testing"
)

func TestConsoleAppConfigLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	send := func(service, operation string, input map[string]any, status int) map[string]any {
		t.Helper()
		return consoleResponse(t, srv, consoleLambdaRequest(t, srv, service, operation, account, region, input), status)
	}

	call := func(service, operation string, input map[string]any) map[string]any {
		t.Helper()
		return send(service, operation, input, http.StatusOK)["output"].(map[string]any)
	}

	appconfig := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return call("appconfig", operation, input)
	}

	refuse := func(operation string, input map[string]any, status int, code string) {
		t.Helper()
		if out := send("appconfig", operation, input, status); out["code"] != code {
			t.Fatal(operation, out)
		}
	}

	items := func(out map[string]any) []any {
		t.Helper()
		list, _ := out["Items"].([]any)
		return list
	}

	var capabilities map[string]any
	if internal(t, srv, http.MethodGet, consoleActionsPath, &capabilities) != http.StatusOK ||
		capabilities["appconfig"] != true || capabilities["appconfigdata"] != true {
		t.Fatal(capabilities)
	}

	// Applications: names are unique, a description is cleared by sending an empty one, and tags go by ARN.
	app := appconfig("CreateApplication", map[string]any{
		"Name": "checkout", "Description": "Checkout service", "Tags": map[string]any{"team": "core"},
	})
	appID := app["Id"].(string)
	if app["Name"] != "checkout" || app["Description"] != "Checkout service" {
		t.Fatal(app)
	}

	refuse("CreateApplication", map[string]any{"Name": "checkout"}, http.StatusConflict, "ConflictException")
	appconfig("CreateApplication", map[string]any{"Name": "billing"})
	page := appconfig("ListApplications", map[string]any{"MaxResults": 1})
	if len(items(page)) != 1 || page["NextToken"] == nil {
		t.Fatal(page)
	}

	rest := appconfig("ListApplications", map[string]any{"MaxResults": 1, "NextToken": page["NextToken"]})
	if len(items(rest)) != 1 || rest["NextToken"] != nil {
		t.Fatal(rest)
	}

	cleared := appconfig("UpdateApplication", map[string]any{"ApplicationId": appID, "Description": ""})
	if cleared["Description"] != "" || cleared["Name"] != "checkout" {
		t.Fatal(cleared)
	}

	arn := "arn:aws:appconfig:" + region + ":" + account + ":application/" + appID
	appconfig("TagResource", map[string]any{"ResourceArn": arn, "Tags": map[string]any{"env": "dev"}})
	appconfig("UntagResource", map[string]any{"ResourceArn": arn, "TagKeys": []any{"team"}})
	tags := appconfig("ListTagsForResource", map[string]any{"ResourceArn": arn})["Tags"].(map[string]any)
	if len(tags) != 1 || tags["env"] != "dev" {
		t.Fatal(tags)
	}

	// Environments start ready for deployment; CloudWatch alarm monitors are not a member of the service.
	env := appconfig("CreateEnvironment", map[string]any{
		"ApplicationId": appID, "Name": "prod", "Description": "Production",
	})
	envID := env["Id"].(string)
	if env["State"] != "READY_FOR_DEPLOYMENT" || env["Name"] != "prod" {
		t.Fatal(env)
	}

	refuse("CreateEnvironment", map[string]any{"ApplicationId": appID, "Name": "prod"},
		http.StatusConflict, "ConflictException")
	refuse("CreateEnvironment", map[string]any{
		"ApplicationId": appID, "Name": "alarmed",
		"Monitors": []any{map[string]any{"AlarmArn": "arn:aws:cloudwatch:" + region + ":" + account + ":alarm:a"}},
	}, http.StatusBadRequest, "ValidationException")
	if renamed := appconfig("UpdateEnvironment", map[string]any{
		"ApplicationId": appID, "EnvironmentId": envID, "Name": "production",
	}); renamed["Name"] != "production" || renamed["Description"] != "Production" {
		t.Fatal(renamed)
	}

	// Configuration profiles are hosted, typed, and carry validators that edits can replace or clear.
	profile := appconfig("CreateConfigurationProfile", map[string]any{
		"ApplicationId": appID, "Name": "flags", "Description": "Feature flags", "LocationUri": "hosted",
		"Type":       "AWS.AppConfig.FeatureFlags",
		"Validators": []any{map[string]any{"Type": "JSON_SCHEMA", "Content": `{"type":"object"}`}},
	})
	profileID := profile["Id"].(string)
	if profile["Type"] != "AWS.AppConfig.FeatureFlags" || profile["LocationUri"] != "hosted" ||
		len(profile["Validators"].([]any)) != 1 {
		t.Fatal(profile)
	}

	refuse("CreateConfigurationProfile", map[string]any{
		"ApplicationId": appID, "Name": "external", "LocationUri": "s3://bucket/key",
	}, http.StatusBadRequest, "BadRequestException")
	refuse("CreateConfigurationProfile", map[string]any{
		"ApplicationId": appID, "Name": "flags", "LocationUri": "hosted",
	}, http.StatusConflict, "ConflictException")
	freeform := appconfig("CreateConfigurationProfile", map[string]any{
		"ApplicationId": appID, "Name": "settings", "LocationUri": "hosted",
	})
	if freeform["Type"] != "AWS.Freeform" {
		t.Fatal(freeform)
	}

	summaries := items(appconfig("ListConfigurationProfiles", map[string]any{"ApplicationId": appID}))
	typed := items(appconfig("ListConfigurationProfiles", map[string]any{
		"ApplicationId": appID, "Type": "AWS.AppConfig.FeatureFlags",
	}))
	if len(summaries) != 2 || len(typed) != 1 ||
		typed[0].(map[string]any)["ValidatorTypes"].([]any)[0] != "JSON_SCHEMA" {
		t.Fatal(summaries, typed)
	}

	appconfig("UpdateConfigurationProfile", map[string]any{
		"ApplicationId": appID, "ConfigurationProfileId": profileID, "Description": "Flags v2",
		"Validators": []any{},
	})
	got := appconfig("GetConfigurationProfile", map[string]any{
		"ApplicationId": appID, "ConfigurationProfileId": profileID,
	})
	validators, _ := got["Validators"].([]any)
	if got["Description"] != "Flags v2" || got["Name"] != "flags" || len(validators) != 0 {
		t.Fatal(got)
	}

	// Hosted versions keep their bytes, numbers and unique labels; content travels as base64.
	binary := base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 255})
	text := base64.StdEncoding.EncodeToString([]byte(`{"flags":{"checkout":{"enabled":true}}}`))
	version := func(content, contentType, label string) map[string]any {
		t.Helper()
		return appconfig("CreateHostedConfigurationVersion", map[string]any{
			"ApplicationId": appID, "ConfigurationProfileId": profileID, "Content": content,
			"ContentType": contentType, "Description": "version " + label, "VersionLabel": label,
		})
	}

	first, second := version(binary, "application/octet-stream", "first"), version(text, "application/json", "second")
	if first["VersionNumber"] != float64(1) || first["Content"] != binary || second["VersionNumber"] != float64(2) {
		t.Fatal(first, second)
	}

	refuse("CreateHostedConfigurationVersion", map[string]any{
		"ApplicationId": appID, "ConfigurationProfileId": profileID, "Content": text,
		"ContentType": "application/json", "VersionLabel": "first",
	}, http.StatusConflict, "ConflictException")
	refuse("CreateHostedConfigurationVersion", map[string]any{
		"ApplicationId": appID, "ConfigurationProfileId": profileID, "Content": "not base64!",
		"ContentType": "text/plain",
	}, http.StatusBadRequest, "ValidationException")
	viewed := appconfig("GetHostedConfigurationVersion", map[string]any{
		"ApplicationId": appID, "ConfigurationProfileId": profileID, "VersionNumber": 2,
	})
	if viewed["Content"] != text || viewed["ContentType"] != "application/json" || viewed["VersionLabel"] != "second" ||
		viewed["Description"] != "version second" {
		t.Fatal(viewed)
	}

	versions := items(appconfig("ListHostedConfigurationVersions", map[string]any{
		"ApplicationId": appID, "ConfigurationProfileId": profileID,
	}))
	if len(versions) != 2 || versions[0].(map[string]any)["Content"] != nil {
		t.Fatal(versions)
	}

	// Deployment strategies: the four AWS ones are immutable, and a custom one always names ReplicateTo.
	strategies := items(appconfig("ListDeploymentStrategies", map[string]any{}))
	if len(strategies) != 4 || strategies[0].(map[string]any)["Id"] == nil {
		t.Fatal(strategies)
	}

	custom := appconfig("CreateDeploymentStrategy", map[string]any{
		"Name": "gradual", "Description": "Slow", "DeploymentDurationInMinutes": 30, "FinalBakeTimeInMinutes": 10,
		"GrowthFactor": 25.5, "GrowthType": "EXPONENTIAL", "ReplicateTo": "NONE",
	})
	strategyID := custom["Id"].(string)
	if custom["GrowthFactor"] != 25.5 || custom["GrowthType"] != "EXPONENTIAL" ||
		custom["DeploymentDurationInMinutes"] != float64(30) {
		t.Fatal(custom)
	}

	refuse("CreateDeploymentStrategy", map[string]any{
		"Name": "unreplicated", "DeploymentDurationInMinutes": 1, "GrowthFactor": 10,
	}, http.StatusBadRequest, "BadRequestException")
	refuse("CreateDeploymentStrategy", map[string]any{
		"Name": "too-fast", "DeploymentDurationInMinutes": 1, "GrowthFactor": 101, "ReplicateTo": "NONE",
	}, http.StatusBadRequest, "BadRequestException")
	refuse("UpdateDeploymentStrategy", map[string]any{
		"DeploymentStrategyId": "AppConfig.AllAtOnce", "Description": "changed",
	}, http.StatusBadRequest, "BadRequestException")
	refuse("DeleteDeploymentStrategy", map[string]any{"DeploymentStrategyId": "AppConfig.AllAtOnce"},
		http.StatusBadRequest, "BadRequestException")
	if tuned := appconfig("UpdateDeploymentStrategy", map[string]any{
		"DeploymentStrategyId": strategyID, "GrowthType": "LINEAR", "GrowthFactor": 50, "Description": "",
		"DeploymentDurationInMinutes": 5, "FinalBakeTimeInMinutes": 0,
	}); tuned["GrowthType"] != "LINEAR" || tuned["Description"] != "" || tuned["FinalBakeTimeInMinutes"] != float64(0) {
		t.Fatal(tuned)
	}

	// Deployments finish at once; the page reads their state and percentage, and a fetch session sees the content.
	deploy := func(version, strategy string) map[string]any {
		t.Helper()
		return appconfig("StartDeployment", map[string]any{
			"ApplicationId": appID, "EnvironmentId": envID, "ConfigurationProfileId": profileID,
			"ConfigurationVersion": version, "DeploymentStrategyId": strategy, "Description": "rollout " + version,
		})
	}

	one := deploy("2", strategyID)
	if one["DeploymentNumber"] != float64(1) || one["State"] != "COMPLETE" ||
		one["PercentageComplete"] != float64(100) {
		t.Fatal(one)
	}

	if one["ConfigurationName"] != "flags" || one["ConfigurationVersion"] != "2" || one["VersionLabel"] != "second" {
		t.Fatal(one)
	}

	if one["StartedAt"] == nil || one["CompletedAt"] == nil || one["GrowthType"] != "LINEAR" {
		t.Fatal(one)
	}

	refuse("StartDeployment", map[string]any{
		"ApplicationId": appID, "EnvironmentId": envID, "ConfigurationProfileId": profileID,
		"ConfigurationVersion": "9", "DeploymentStrategyId": strategyID,
	}, http.StatusNotFound, "ResourceNotFoundException")

	session := call("appconfigdata", "StartConfigurationSession", map[string]any{
		"ApplicationIdentifier": appID, "EnvironmentIdentifier": envID, "ConfigurationProfileIdentifier": profileID,
	})
	token := session["InitialConfigurationToken"].(string)
	fetched := call("appconfigdata", "GetLatestConfiguration", map[string]any{"ConfigurationToken": token})
	if fetched["Configuration"] != text || fetched["ContentType"] != "application/json" ||
		fetched["VersionLabel"] != "second" || fetched["NextPollIntervalInSeconds"] != float64(60) ||
		fetched["NextPollConfigurationToken"] == nil {
		t.Fatal(fetched)
	}

	// A token is spent by its poll; the next one reports no change by leaving the content out.
	if out := send("appconfigdata", "GetLatestConfiguration", map[string]any{"ConfigurationToken": token},
		http.StatusBadRequest); out["code"] != "BadRequestException" {
		t.Fatal(out)
	}

	unchanged := call("appconfigdata", "GetLatestConfiguration", map[string]any{
		"ConfigurationToken": fetched["NextPollConfigurationToken"],
	})
	if unchanged["Configuration"] != nil {
		t.Fatal(unchanged)
	}

	if out := send("appconfigdata", "StartConfigurationSession", map[string]any{
		"ApplicationIdentifier": appID, "EnvironmentIdentifier": "missing", "ConfigurationProfileIdentifier": profileID,
	}, http.StatusNotFound); out["code"] != "ResourceNotFoundException" {
		t.Fatal(out)
	}

	two := deploy("1", "AppConfig.AllAtOnce")
	if two["DeploymentNumber"] != float64(2) || two["ConfigurationVersion"] != "1" {
		t.Fatal(two)
	}

	listed := items(appconfig("ListDeployments", map[string]any{"ApplicationId": appID, "EnvironmentId": envID}))
	detail := appconfig("GetDeployment", map[string]any{
		"ApplicationId": appID, "EnvironmentId": envID, "DeploymentNumber": 1,
	})
	if len(listed) != 2 || listed[0].(map[string]any)["State"] != "COMPLETE" ||
		detail["DeploymentStrategyId"] != strategyID || detail["Description"] != "rollout 2" ||
		detail["ConfigurationLocationUri"] != "hosted" {
		t.Fatal(listed, detail)
	}

	// Stopping reverts the latest completed deployment and needs AllowRevert.
	stop := map[string]any{"ApplicationId": appID, "EnvironmentId": envID, "DeploymentNumber": 2}
	refuse("StopDeployment", stop, http.StatusBadRequest, "BadRequestException")
	refuse("StopDeployment", map[string]any{
		"ApplicationId": appID, "EnvironmentId": envID, "DeploymentNumber": 1, "AllowRevert": true,
	}, http.StatusConflict, "ConflictException")
	stop["AllowRevert"] = true
	if reverted := appconfig("StopDeployment", stop); reverted["State"] != "REVERTED" {
		t.Fatal(reverted)
	}

	environment := appconfig("GetEnvironment", map[string]any{"ApplicationId": appID, "EnvironmentId": envID})
	if environment["State"] != "REVERTED" {
		t.Fatal(environment)
	}

	// The reverted deployment's version is free to delete; the one that is live again is not.
	refuse("DeleteHostedConfigurationVersion", map[string]any{
		"ApplicationId": appID, "ConfigurationProfileId": profileID, "VersionNumber": 2,
	}, http.StatusConflict, "ConflictException")
	appconfig("DeleteHostedConfigurationVersion", map[string]any{
		"ApplicationId": appID, "ConfigurationProfileId": profileID, "VersionNumber": 1,
	})

	// Parents wait for their children; deleting an environment removes its deployments.
	refuse("DeleteConfigurationProfile", map[string]any{
		"ApplicationId": appID, "ConfigurationProfileId": profileID,
	}, http.StatusConflict, "ConflictException")
	refuse("DeleteApplication", map[string]any{"ApplicationId": appID}, http.StatusConflict, "ConflictException")
	appconfig("DeleteEnvironment", map[string]any{"ApplicationId": appID, "EnvironmentId": envID})
	appconfig("DeleteHostedConfigurationVersion", map[string]any{
		"ApplicationId": appID, "ConfigurationProfileId": profileID, "VersionNumber": 2,
	})
	appconfig("DeleteConfigurationProfile", map[string]any{"ApplicationId": appID, "ConfigurationProfileId": profileID})
	appconfig("DeleteConfigurationProfile", map[string]any{
		"ApplicationId": appID, "ConfigurationProfileId": freeform["Id"],
	})
	appconfig("DeleteDeploymentStrategy", map[string]any{"DeploymentStrategyId": strategyID})
	appconfig("DeleteApplication", map[string]any{"ApplicationId": appID})
	refuse("GetApplication", map[string]any{"ApplicationId": appID}, http.StatusNotFound, "ResourceNotFoundException")
	if left := items(appconfig("ListApplications", map[string]any{})); len(left) != 1 {
		t.Fatal(left)
	}

	// Implemented operations outside this page stay unreachable through console actions.
	refuse("GetDeploymentStrategy", map[string]any{"DeploymentStrategyId": "AppConfig.AllAtOnce"},
		http.StatusBadRequest, "UnknownOperationException")
}
