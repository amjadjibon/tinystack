import assert from "node:assert/strict";
import test from "node:test";
import {
	deleteInput,
	resourceTagPlan,
	tagChanges,
	taggingFailure,
} from "../src/lib/genericResources.ts";

const resource = {
	service: "route53",
	type: "hosted-zone",
	name: "example.com.",
	arn: "arn:aws:route53:::hostedzone/Z123",
	account: "000000000000",
	region: "",
	tags: { old: "v", keep: "same" },
};

test("delete identifiers use the advertised mapping, never display names for zone IDs", () => {
	for (const [identifier, expected] of [
		["name", resource.name],
		["arn", resource.arn],
		["arn-suffix", "Z123"],
	] as const) {
		assert.deepEqual(
			deleteInput(resource, {
				service: "route53",
				operation: "DeleteHostedZone",
				field: "Id",
				identifier,
			}),
			{ Id: expected },
		);
	}
	assert.throws(() =>
		deleteInput(
			{ ...resource, arn: "" },
			{
				service: "route53",
				operation: "DeleteHostedZone",
				field: "Id",
				identifier: "arn-suffix",
			},
		),
	);
	assert.throws(() =>
		deleteInput(resource, {
			service: "ssm",
			operation: "DeleteParameter",
			field: "Name",
			identifier: "name",
		}),
	);
});
test("tag diffs preserve unchanged values and enforce shared limits by Unicode code point", () => {
	assert.deepEqual(tagChanges(resource.tags, { keep: "same", new: "x" }), {
		removed: ["old"],
		added: { new: "x" },
	});
	for (const tags of [
		{ "": "x" },
		{ "AWS:reserved": "x" },
		{ ["k".repeat(129)]: "x" },
		{ key: "v".repeat(257) },
		Object.fromEntries(Array.from({ length: 51 }, (_, i) => [`k${i}`, "v"])),
	])
		assert.throws(() => tagChanges({}, tags));
	assert.doesNotThrow(() =>
		tagChanges({}, { ["😀".repeat(128)]: "😀".repeat(256) }),
	);
});
test("tagging plans remove first and surface per-resource failures", async () => {
	const calls: unknown[][] = [];
	const steps = resourceTagPlan(
		async (...args: unknown[]) => {
			calls.push(args);
			return {} as never;
		},
		resource,
		"us-east-1",
		{ keep: "same", new: "x" },
	);
	for (const step of steps) await step.run();
	assert.deepEqual(
		calls.map((c) => [c[1], c[3]]),
		[
			["UntagResources", { ResourceARNList: [resource.arn], TagKeys: ["old"] }],
			["TagResources", { ResourceARNList: [resource.arn], Tags: { new: "x" } }],
		],
	);
	assert.throws(
		() =>
			taggingFailure({
				FailedResourcesMap: {
					[resource.arn]: {
						ErrorCode: "AccessDenied",
						ErrorMessage: "Denied verbatim",
					},
				},
			}),
		/Z123: AccessDenied: Denied verbatim/,
	);
	assert.deepEqual(
		resourceTagPlan(
			async () => ({}) as never,
			resource,
			"us-east-1",
			resource.tags,
		),
		[],
	);
});
