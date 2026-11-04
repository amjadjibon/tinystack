import assert from "node:assert/strict";
import test from "node:test";
import {
	codebuildHref,
	pending,
	projectForm,
	projectInput,
	variables,
} from "../src/lib/codebuildHelpers.ts";

test("project inputs preserve inline buildspec and reference variables", () => {
	const f = projectForm();
	f.name = "my-build";
	f.role = "arn:aws:iam::000000000000:role/build";
	f.vars = '[{"Name":"TOKEN","Value":"secret:key","Type":"SECRETS_MANAGER"}]';
	f.tags = '{"team":"console"}';
	const input = projectInput(f);
	assert.equal(input.Source.Location, undefined);
	assert.match(input.Source.Buildspec, /\n/);
	assert.deepEqual(input.Environment.EnvironmentVariables, [
		{ Name: "TOKEN", Value: "secret:key", Type: "SECRETS_MANAGER" },
	]);
	assert.deepEqual(input.Tags, [{ Key: "team", Value: "console" }]);
	assert.equal(input.TimeoutInMinutes, 60);
	assert.throws(() => projectInput({ ...f, timeout: "4" }), /Timeout/);
	assert.throws(() => projectInput({ ...f, queued: "481" }), /Queued/);
	assert.throws(() => projectInput({ ...f, name: "x" }), /Name/);
	assert.throws(() => projectInput({ ...f, artifacts: "[]" }), /JSON object/);
});
test("environment overrides reject ambiguous or unsupported values", () => {
	assert.deepEqual(variables('[{"Name":"X","Value":""}]'), [
		{ Name: "X", Value: "", Type: "PLAINTEXT" },
	]);
	for (const text of [
		"{}",
		'[{"Name":"CODEBUILD_X","Value":"x"}]',
		'[{"Name":"X","Value":2}]',
		'[{"Name":"X","Value":"x","Type":"UNKNOWN"}]',
		'[{"Name":"X","Value":"x"},{"Name":"X","Value":"y"}]',
	])
		assert.throws(() => variables(text));
});
test("links encode identifiers and only retain scope and explicit navigation", () => {
	const url = new URL(
		codebuildHref(
			{ account: "000000000000", region: "eu-west-1" },
			"my-build",
			"my-build:a/b",
			"builds",
		),
		"http://localhost",
	);
	assert.equal(url.searchParams.get("build"), "my-build:a/b");
	assert.equal(url.searchParams.get("tab"), "builds");
	assert.deepEqual([...url.searchParams.keys()].sort(), [
		"account",
		"build",
		"project",
		"region",
		"tab",
	]);
	assert.equal(
		pending({ Id: "x", ProjectName: "x", BuildStatus: "IN_PROGRESS" }),
		true,
	);
	assert.equal(
		pending({ Id: "x", ProjectName: "x", BuildStatus: "FAILED" }),
		false,
	);
});

test("report group input validates names, uses shared tags and preserves the ARN for updates", async () => {
	const { reportGroupInput } = await import("../src/lib/codebuildHelpers.ts");
	assert.deepEqual(reportGroupInput("my-tests", '{"team":"console"}'), {
		Name: "my-tests",
		Type: "TEST",
		ExportConfig: { ExportConfigType: "NO_EXPORT" },
		Tags: [{ Key: "team", Value: "console" }],
	});
	assert.throws(() => reportGroupInput("x", "{}"), /name/);
	assert.throws(() => reportGroupInput("tests", "[]"), /JSON/);
	assert.deepEqual(reportGroupInput("ignored", "{}", "arn:group"), {
		Arn: "arn:group",
		ExportConfig: { ExportConfigType: "NO_EXPORT" },
		Tags: [],
	});
});
