import assert from "node:assert/strict";
import test from "node:test";
import {
	cfnDate,
	filterStacks,
	findInventory,
	inProgress,
	statusType,
} from "../src/lib/cloudformationHelpers.ts";

test("stack statuses map to indicators and only running operations are followed", () => {
	const expected: Record<string, [string, boolean]> = {
		CREATE_COMPLETE: ["success", false],
		UPDATE_COMPLETE_CLEANUP_IN_PROGRESS: ["in-progress", true],
		DELETE_IN_PROGRESS: ["in-progress", true],
		ROLLBACK_COMPLETE: ["warning", false],
		UPDATE_ROLLBACK_IN_PROGRESS: ["in-progress", true],
		CREATE_FAILED: ["error", false],
		DELETE_FAILED: ["error", false],
		UPDATE_ROLLBACK_FAILED: ["error", false],
		DELETE_COMPLETE: ["stopped", false],
		REVIEW_IN_PROGRESS: ["pending", false],
	};
	for (const [status, [type, running]] of Object.entries(expected)) {
		assert.equal(statusType(status), type, status);
		assert.equal(inProgress(status), running, status);
	}
});

test("deleted stacks are hidden until included or filtered by name", () => {
	const stacks = [
		{ StackStatus: "CREATE_COMPLETE" },
		{ StackStatus: "DELETE_COMPLETE" },
		{ StackStatus: "CREATE_FAILED" },
	];
	assert.equal(filterStacks(stacks, "", false).length, 2);
	assert.equal(filterStacks(stacks, "", true).length, 3);
	assert.deepEqual(filterStacks(stacks, "CREATE_FAILED", false), [stacks[2]]);
	assert.deepEqual(filterStacks(stacks, "DELETE_COMPLETE", false), [stacks[1]]);
});

test("timestamps accept RFC 3339 text and epoch seconds", () => {
	assert.equal(cfnDate("2026-10-31T01:02:03.5Z"), "2026-10-31T01:02:03.500Z");
	assert.equal(cfnDate(0), "1970-01-01T00:00:00.000Z");
	assert.equal(cfnDate(undefined), "—");
	assert.equal(cfnDate("not a date"), "—");
});

test("physical IDs resolve inside the stack scope and prefer the matching service", () => {
	const record = (
		service: string,
		region: string,
		account = "000000000000",
	) => ({
		service,
		type: "x",
		account,
		region,
		name: "orders",
		arn: `arn:${service}:orders`,
	});
	const inventory = [
		record("sqs", "us-east-1"),
		record("sns", "us-east-1"),
		record("sns", "eu-west-1"),
		record("sns", "us-east-1", "111111111111"),
	];
	const topic = {
		PhysicalResourceId: "orders",
		ResourceType: "AWS::SNS::Topic",
	};
	assert.equal(
		findInventory(inventory, topic, "000000000000", "us-east-1"),
		inventory[1],
	);
	assert.equal(
		findInventory(
			inventory,
			{ ...topic, PhysicalResourceId: "arn:sqs:orders" },
			"000000000000",
			"us-east-1",
		),
		inventory[0],
	);
	assert.equal(
		findInventory(inventory, topic, "222222222222", "us-east-1"),
		undefined,
	);
	assert.equal(
		findInventory(
			inventory,
			{ ...topic, PhysicalResourceId: "" },
			"000000000000",
			"us-east-1",
		),
		undefined,
	);
});
