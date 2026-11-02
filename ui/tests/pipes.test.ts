import assert from "node:assert/strict";
import test from "node:test";
import {
	emptyPipeForm,
	formFromPipe,
	pipeInput,
} from "../src/lib/pipesHelpers.ts";

const form = {
	...emptyPipeForm(),
	name: "orders",
	source: "arn:aws:sqs:us-east-1:000000000000:orders",
	target: "arn:aws:lambda:us-east-1:000000000000:function:process",
	role: "arn:aws:iam::000000000000:role/pipes",
};
test("create includes supported configuration and tags", () => {
	const input = pipeInput({
		...form,
		description: "Orders",
		sourceParameters: JSON.stringify({
			SqsQueueParameters: { BatchSize: 5 },
			FilterCriteria: { Filters: [{ Pattern: '{"body":{"kind":["order"]}}' }] },
		}),
		enrichment: form.target,
		enrichmentParameters: '{"InputTemplate":"<$.body>"}',
		targetParameters:
			'{"LambdaFunctionParameters":{"InvocationType":"REQUEST_RESPONSE"}}',
		tags: '{"env":"dev"}',
	});
	assert.equal(input.Source, form.source);
	assert.equal(input.Name, "orders");
	assert.equal(input.DesiredState, "STOPPED");
	assert.deepEqual(input.Tags, { env: "dev" });
	assert.deepEqual(input.EnrichmentParameters, { InputTemplate: "<$.body>" });
	assert.deepEqual(input.TargetParameters, {
		LambdaFunctionParameters: { InvocationType: "REQUEST_RESPONSE" },
	});
});
test("edit preserves source and stream starting position without mutating detail", () => {
	const detail = {
		Name: form.name,
		Source: form.source,
		Target: form.target,
		RoleArn: form.role,
		SourceParameters: {
			KinesisStreamParameters: { StartingPosition: "LATEST", BatchSize: 10 },
		},
	};
	const edit = formFromPipe(detail);
	const input = pipeInput(edit, false);
	assert.equal(Object.hasOwn(input, "Source"), false);
	assert.equal(Object.hasOwn(input, "Tags"), false);
	assert.deepEqual(input.SourceParameters, {
		KinesisStreamParameters: { BatchSize: 10 },
	});
	assert.equal(
		detail.SourceParameters.KinesisStreamParameters.StartingPosition,
		"LATEST",
	);
	assert.equal(input.Description, "");
	assert.equal(input.Enrichment, "");
	assert.deepEqual(input.TargetParameters, {});
});
test("invalid JSON, unsupported fields and unavailable source options fail before submission", () => {
	for (const patch of [
		{ name: "bad/name" },
		{ source: "" },
		{ role: "" },
		{ desiredState: "DELETED" },
		{ tags: '{"a":1}' },
		{ sourceParameters: "[]" },
		{ targetParameters: "{" },
		{ enrichmentParameters: '{"HttpParameters":{}}' },
		{ targetParameters: '{"EcsTaskParameters":{}}' },
		{
			sourceParameters:
				'{"KinesisStreamParameters":{"StartingPositionTimestamp":12}}',
		},
		{
			sourceParameters:
				'{"SqsQueueParameters":{"MaximumBatchingWindowInSeconds":1}}',
		},
		{
			sourceParameters:
				'{"FilterCriteria":{"Filters":[{"Pattern":"invalid"}]}}',
		},
	])
		assert.throws(() => pipeInput({ ...form, ...patch }));
	assert.throws(() =>
		pipeInput(
			{
				...form,
				sourceParameters:
					'{"KinesisStreamParameters":{"StartingPosition":"LATEST"}}',
			},
			false,
		),
	);
});
test("stream create retains position and target blocks round trip", () => {
	for (const key of ["KinesisStreamParameters", "DynamoDBStreamParameters"]) {
		const params = {
			[key]: {
				StartingPosition: "TRIM_HORIZON",
				BatchSize: 100,
				MaximumBatchingWindowInSeconds: 0,
			},
		};
		assert.deepEqual(
			pipeInput({ ...form, sourceParameters: JSON.stringify(params) })
				.SourceParameters,
			params,
		);
	}
	for (const target of [
		{ KinesisStreamParameters: { PartitionKey: "orders" } },
		{
			SqsQueueParameters: {
				MessageGroupId: "group",
				MessageDeduplicationId: "id",
			},
		},
		{
			EventBridgeEventBusParameters: {
				Source: "orders",
				DetailType: "created",
			},
		},
		{
			StepFunctionStateMachineParameters: { InvocationType: "FIRE_AND_FORGET" },
		},
	])
		assert.deepEqual(
			pipeInput({ ...form, targetParameters: JSON.stringify(target) })
				.TargetParameters,
			target,
		);
});
