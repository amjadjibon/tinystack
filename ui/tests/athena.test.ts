import assert from "node:assert/strict";
import test from "node:test";
import {
	batches,
	namedQueryInput,
	queryInput,
	rerunOutput,
	resultRows,
	terminal,
	workgroupInput,
	workgroupSettings,
} from "../src/lib/athenaHelpers.ts";
import { unixTime } from "../src/lib/format.ts";

test("query requests only send supported fields and optional overrides", () => {
	assert.deepEqual(
		queryInput("SELECT ?", "primary", "AwsDataCatalog", "db", "", ["'hi'"]),
		{
			QueryString: "SELECT ?",
			WorkGroup: "primary",
			QueryExecutionContext: { Catalog: "AwsDataCatalog", Database: "db" },
			ExecutionParameters: ["'hi'"],
		},
	);
	assert.throws(() => queryInput(" ", "primary", "AwsDataCatalog", "", ""));
	assert.throws(() =>
		queryInput("é".repeat(131073), "primary", "AwsDataCatalog", "", ""),
	);
	assert.deepEqual(
		queryInput("SELECT 1", "primary", "AwsDataCatalog", "", " s3://bucket/ ")
			.ResultConfiguration,
		{ OutputLocation: "s3://bucket/" },
	);
});
test("results remove only first DML header and preserve null and empty strings", () => {
	const result = {
		ResultSet: {
			Rows: [
				{ Data: [{ VarCharValue: "col" }] },
				{ Data: [{}, { VarCharValue: "" }] },
			],
		},
	};
	assert.deepEqual(resultRows(result, true, "DML"), [[null, ""]]);
	assert.deepEqual(resultRows(result, false, "DML"), [["col"], [null, ""]]);
	assert.deepEqual(resultRows(result, true, "DDL"), [["col"], [null, ""]]);
	assert.deepEqual(resultRows({}, true, "DML"), []);
});
test("terminal states, batching and epoch timestamps", () => {
	for (const state of ["SUCCEEDED", "FAILED", "CANCELLED"])
		assert.equal(terminal(state), true);
	for (const state of [undefined, "QUEUED", "RUNNING"])
		assert.equal(terminal(state), false);
	assert.deepEqual(
		batches(Array.from({ length: 101 }, (_, i) => i)).map((a) => a.length),
		[50, 50, 1],
	);
	assert.deepEqual(batches([]), []);
	assert.throws(() => batches([1], 0));
	assert.equal(unixTime(undefined), "-");
	assert.equal(
		unixTime(1700000000.25),
		new Date(1700000000250).toLocaleString(),
	);
});
test("named query validation and workgroup create/update settings", () => {
	assert.throws(() => namedQueryInput("name", "", "SELECT 1", "", "primary"));
	assert.throws(() => namedQueryInput("", "", "SELECT 1", "db", "primary"));
	assert.deepEqual(
		namedQueryInput(" name ", "desc", "SELECT 1", "db", "primary"),
		{
			Name: "name",
			Description: "desc",
			QueryString: "SELECT 1",
			Database: "db",
			WorkGroup: "primary",
		},
	);
	assert.throws(() =>
		workgroupInput("bad name", "", "", true, false, workgroupSettings()),
	);
	assert.throws(() =>
		workgroupInput(
			"good",
			"",
			"https://bucket",
			true,
			false,
			workgroupSettings(),
		),
	);
	assert.deepEqual(
		workgroupInput("good", "desc", "", false, true, workgroupSettings()),
		{
			WorkGroup: "good",
			Description: "desc",
			ConfigurationUpdates: {
				PublishCloudWatchMetricsEnabled: true,
				RequesterPaysEnabled: false,
				EngineVersion: { SelectedEngineVersion: "AUTO" },
				RemoveBytesScannedCutoffPerQuery: true,
				EnforceWorkGroupConfiguration: false,
				ResultConfigurationUpdates: {
					RemoveOutputLocation: true,
					RemoveEncryptionConfiguration: true,
					RemoveExpectedBucketOwner: true,
				},
			},
		},
	);
	assert.deepEqual(
		workgroupInput(
			"good",
			"",
			"s3://bucket/",
			true,
			false,
			workgroupSettings(),
		),
		{
			Name: "good",
			Description: "",
			Configuration: {
				PublishCloudWatchMetricsEnabled: true,
				RequesterPaysEnabled: false,
				EngineVersion: { SelectedEngineVersion: "AUTO" },
				EnforceWorkGroupConfiguration: true,
				ResultConfiguration: { OutputLocation: "s3://bucket/" },
			},
		},
	);
});

test("workgroup encryption, removals and configuration round trip", () => {
	const settings = workgroupSettings({
		Name: "test",
		Configuration: {
			ResultConfiguration: {
				EncryptionConfiguration: { EncryptionOption: "SSE_KMS", KmsKey: "key" },
				ExpectedBucketOwner: "123456789012",
			},
			BytesScannedCutoffPerQuery: 123,
			PublishCloudWatchMetricsEnabled: false,
			RequesterPaysEnabled: true,
			EngineVersion: { SelectedEngineVersion: "Athena engine version 3" },
		},
	});
	const input = workgroupInput(
		"test",
		"",
		"s3://bucket/",
		false,
		true,
		settings,
	);
	assert.deepEqual(input.ConfigurationUpdates?.ResultConfigurationUpdates, {
		OutputLocation: "s3://bucket/",
		EncryptionConfiguration: { EncryptionOption: "SSE_KMS", KmsKey: "key" },
		ExpectedBucketOwner: "123456789012",
	});
	assert.equal(input.ConfigurationUpdates?.BytesScannedCutoffPerQuery, 123);
	assert.equal(
		input.ConfigurationUpdates?.PublishCloudWatchMetricsEnabled,
		false,
	);
	assert.equal(input.ConfigurationUpdates?.RequesterPaysEnabled, true);
	assert.throws(() =>
		workgroupInput("test", "", "", false, true, { ...settings, kmsKey: "" }),
	);
	assert.throws(() =>
		workgroupInput("test", "", "", false, true, { ...settings, owner: "bad" }),
	);
	assert.throws(() =>
		workgroupInput("test", "", "", false, true, { ...settings, cutoff: "1.5" }),
	);
});

test("history re-runs preserve the output prefix for successful and failed executions", () => {
	assert.equal(
		rerunOutput({
			QueryExecutionId: "id",
			ResultConfiguration: { OutputLocation: "s3://bucket/path/id.csv" },
		}),
		"s3://bucket/path/",
	);
	assert.equal(
		rerunOutput({
			QueryExecutionId: "id",
			ResultConfiguration: { OutputLocation: "s3://bucket/id.txt" },
		}),
		"s3://bucket/",
	);
	assert.equal(
		rerunOutput({
			QueryExecutionId: "id",
			ResultConfiguration: { OutputLocation: "s3://bucket/path/" },
		}),
		"s3://bucket/path/",
	);
	assert.equal(rerunOutput(), "");
});
