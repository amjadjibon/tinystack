import assert from "node:assert/strict";
import test from "node:test";
import {
	emptyGlueDefinition,
	glueDefinition,
	glueIdentity,
	glueInput,
	glueKinds,
} from "../src/lib/glueHelpers.ts";

test("catalog inputs normalize names and retain nested schema and storage", () => {
	const table = {
		...emptyGlueDefinition("Table"),
		Name: "Events",
		StorageDescriptor: {
			Columns: [{ Name: "id", Type: "string" }],
			Location: "s3://data/",
			SerdeInfo: { SerializationLibrary: "serde", Parameters: { a: "b" } },
		},
		PartitionKeys: [{ Name: "day", Type: "string" }],
	};
	assert.deepEqual(
		glueInput("Table", JSON.stringify(table), true, {
			DatabaseName: "catalog",
		}),
		{ DatabaseName: "catalog", TableInput: { ...table, Name: "events" } },
	);
	assert.deepEqual(
		glueInput("Partition", '{"Values":["2026"],"Parameters":{}}', true, {
			DatabaseName: "db",
			TableName: "events",
		}),
		{
			DatabaseName: "db",
			TableName: "events",
			PartitionInput: { Values: ["2026"], Parameters: {} },
		},
	);
});
test("edit strips output fields, converts crawler schedule, and builds update envelopes", () => {
	const job = {
		Name: "etl",
		Role: "role",
		Command: { Name: "pythonshell", ScriptLocation: "s3://b/j.py" },
		CreatedOn: 42,
		LastModifiedOn: 43,
	};
	const definition = glueDefinition("Job", job);
	const { Name, ...JobUpdate } = definition;
	assert.deepEqual(
		glueInput("Job", JSON.stringify(definition), false, {}, Name),
		{ JobName: "etl", JobUpdate },
	);
	assert.equal(job.CreatedOn, 42);
	const crawler = glueDefinition("Crawler", {
		Name: "crawl",
		Schedule: { ScheduleExpression: "cron(0/5 * * * ? *)", State: "SCHEDULED" },
		LastCrawl: { Status: "SUCCEEDED" },
	});
	assert.equal(crawler.Schedule, "cron(0/5 * * * ? *)");
	assert.equal("LastCrawl" in crawler, false);
	for (const kind of glueKinds.filter((k) => k !== "Partition")) {
		const d = {
			...glueDefinition(kind, {
				...emptyGlueDefinition(kind),
				Name: "resource",
			}),
		};
		assert.doesNotThrow(() =>
			glueInput(kind, JSON.stringify(d), false, {}, "resource"),
		);
	}
});
test("unsupported fields and malformed definitions fail before submission", () => {
	for (const text of [
		"null",
		"[]",
		"{",
		'{"Name":""}',
		'{"Name":"x","TargetDatabase":{}}',
		'{"Name":"x","StorageDescriptor":{"AdditionalLocations":[]}}',
	])
		assert.throws(() => glueInput("Table", text, true));
	assert.throws(() =>
		glueInput("Crawler", '{"Name":"x","Targets":{"JdbcTargets":[]}}', true),
	);
	assert.throws(() =>
		glueInput(
			"Crawler",
			'{"Name":"x","Targets":{"S3Targets":[{"Path":"s3://b","ConnectionName":"c"}]}}',
			true,
		),
	);
	assert.throws(() => glueInput("Job", '{"Name":"x","Tags":{"env":1}}', true));
	assert.throws(() => glueInput("Partition", '{"Values":[1]}', true));
	assert.throws(() =>
		glueInput("Database", '{"Name":"renamed"}', false, {}, "original"),
	);
});
test("identities use correct request names and preserve partition value boundaries", () => {
	assert.deepEqual(glueIdentity("Job", { Name: "etl" }), { JobName: "etl" });
	assert.deepEqual(
		glueIdentity(
			"Partition",
			{ Values: ["a/b", "c"] },
			{ DatabaseName: "db", TableName: "t" },
		),
		{ DatabaseName: "db", TableName: "t", PartitionValues: ["a/b", "c"] },
	);
});
