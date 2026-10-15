import assert from "node:assert/strict";
import test from "node:test";
import {
	budgetName,
	categoryName,
	expression,
	jsonArray,
	monitorSpecification,
	notification,
	spend,
	subscriberAddresses,
} from "../src/lib/costValidation.ts";

test("budget constraints preserve decimal strings and reject invalid names and thresholds", () => {
	assert.deepEqual(spend("0.00000000000001", "USD"), {
		Amount: "0.00000000000001",
		Unit: "USD",
	});
	for (const amount of ["-1", "1e2", "Infinity", "", "1."])
		assert.throws(() => spend(amount, "USD"));
	for (const name of [
		"",
		"x".repeat(101),
		"budget:bad",
		"bad/action/name",
		"<script>bad</script>",
	])
		assert.throws(() => budgetName(name));
	for (const threshold of ["", "NaN", "Infinity", "-1", "15000000000001"])
		assert.throws(() =>
			notification("ACTUAL", "GREATER_THAN", threshold, "PERCENTAGE"),
		);
});
test("subscribers reject duplicates and mismatched address types", () => {
	assert.deepEqual(
		subscriberAddresses("a@example.com\nb@example.com", "EMAIL"),
		["a@example.com", "b@example.com"],
	);
	assert.throws(() =>
		subscriberAddresses("a@example.com\na@example.com", "EMAIL"),
	);
	assert.throws(() => subscriberAddresses("a@example.com", "SNS"));
	assert.throws(() =>
		subscriberAddresses("arn:aws:sns:us-east-1:000000000000:topic", "EMAIL"),
	);
});
test("threshold expressions enforce impact dimensions and finite values", () => {
	const threshold = (value: string) => ({
		Dimensions: {
			Key: "ANOMALY_TOTAL_IMPACT_ABSOLUTE",
			Values: [value],
			MatchOptions: ["GREATER_THAN_OR_EQUAL"],
		},
	});
	assert.doesNotThrow(() =>
		expression({ Or: [threshold("100"), threshold("50")] }, "threshold"),
	);
	for (const value of ["Infinity", "NaN", "-1", ""])
		assert.throws(() => expression(threshold(value), "threshold"));
	assert.throws(() => expression({ Not: threshold("10") }, "threshold"));
	assert.throws(() =>
		expression(
			{ Dimensions: { Key: "SERVICE", Values: ["EC2"] } },
			"threshold",
		),
	);
});
test("custom and dimensional monitor specifications have different boundaries", () => {
	assert.doesNotThrow(() =>
		monitorSpecification({ Tags: { Key: "team" } }, "DIMENSIONAL", "TAG"),
	);
	assert.throws(() =>
		monitorSpecification(
			{ Tags: { Key: "team", Values: ["a"] } },
			"DIMENSIONAL",
			"TAG",
		),
	);
	assert.doesNotThrow(() =>
		monitorSpecification(
			{ Dimensions: { Key: "LINKED_ACCOUNT", Values: ["000000000000"] } },
			"CUSTOM",
			"",
		),
	);
	assert.throws(() =>
		monitorSpecification(
			{ Dimensions: { Key: "SERVICE", Values: ["EC2"] } },
			"CUSTOM",
			"",
		),
	);
	assert.throws(() =>
		monitorSpecification(
			{ CostCategories: { Key: "team", Values: ["a", "b"] } },
			"CUSTOM",
			"",
		),
	);
});
test("category JSON rejects malformed rules and out-of-range names", () => {
	assert.throws(() => jsonArray("{}", "Rules", 1));
	assert.throws(() => jsonArray("[]", "Rules", 1));
	assert.throws(() => jsonArray("[null]", "Rules", 1));
	assert.equal(categoryName("Compute team"), "Compute team");
	assert.throws(() => categoryName(" leading"));
	assert.throws(() => categoryName("x".repeat(51)));
	assert.throws(() =>
		expression({ Dimensions: { Key: "SERVICE", Values: ["EC2"] } }, "category"),
	);
});
