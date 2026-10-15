import assert from "node:assert/strict";
import test from "node:test";
import {
	budgetName,
	categoryName,
	expression,
	jsonArray,
	monitorSpecification,
	notification,
	required,
	spend,
	subscriberAddresses,
} from "../src/lib/costValidation.ts";

test("required string validation enforces length and character bounds", () => {
	assert.equal(required("valid", "Field"), "valid");
	assert.equal(required("x".repeat(1024), "Field"), "x".repeat(1024));
	assert.equal(required("🚀", "Emoji", 1), "🚀");
	assert.throws(
		() => required("", "Field"),
		/Field requires 1–1024 characters\./,
	);
	assert.throws(
		() => required("x".repeat(1025), "Field"),
		/Field requires 1–1024 characters\./,
	);
	assert.throws(
		() => required("abc", "Field", 2),
		/Field requires 1–2 characters\./,
	);
});

test("budget constraints preserve decimal strings and reject invalid names and thresholds", () => {
	assert.deepEqual(spend("0.00000000000001", "USD"), {
		Amount: "0.00000000000001",
		Unit: "USD",
	});
	assert.deepEqual(spend(".5", "USD"), {
		Amount: ".5",
		Unit: "USD",
	});
	assert.deepEqual(spend("100", "USD"), {
		Amount: "100",
		Unit: "USD",
	});
	for (const amount of ["-1", "1e2", "Infinity", "", "1."])
		assert.throws(() => spend(amount, "USD"));
	for (const unit of ["", "x".repeat(1025)])
		assert.throws(() => spend("10", unit));
	assert.equal(budgetName("Valid-Budget_123"), "Valid-Budget_123");
	for (const name of [
		"",
		"x".repeat(101),
		"budget:bad",
		"budget\\bad",
		"bad/action/name",
		"<script>bad</script>",
		"<SCRIPT>bad</SCRIPT>",
	])
		assert.throws(() => budgetName(name));
	assert.deepEqual(notification("ACTUAL", "GREATER_THAN", "0", "PERCENTAGE"), {
		NotificationType: "ACTUAL",
		ComparisonOperator: "GREATER_THAN",
		Threshold: 0,
		ThresholdType: "PERCENTAGE",
	});
	assert.deepEqual(
		notification("FORECASTED", "LESS_THAN", "15000000000000", "ABSOLUTE_VALUE"),
		{
			NotificationType: "FORECASTED",
			ComparisonOperator: "LESS_THAN",
			Threshold: 15000000000000,
			ThresholdType: "ABSOLUTE_VALUE",
		},
	);
	for (const threshold of [
		"",
		"   ",
		"NaN",
		"Infinity",
		"-1",
		"15000000000001",
	])
		assert.throws(() =>
			notification("ACTUAL", "GREATER_THAN", threshold, "PERCENTAGE"),
		);
});

test("subscribers reject duplicates and mismatched address types", () => {
	assert.deepEqual(
		subscriberAddresses("a@example.com\nb@example.com", "EMAIL"),
		["a@example.com", "b@example.com"],
	);
	assert.deepEqual(
		subscriberAddresses(
			"arn:aws:sns:us-east-1:123456789012:topic\narn:aws:sns:us-east-1:123456789012:topic.fifo",
			"SNS",
		),
		[
			"arn:aws:sns:us-east-1:123456789012:topic",
			"arn:aws:sns:us-east-1:123456789012:topic.fifo",
		],
	);
	for (const empty of ["", "   \n  \t "])
		assert.throws(() => subscriberAddresses(empty, "EMAIL"));
	assert.throws(() =>
		subscriberAddresses("a@example.com\na@example.com", "EMAIL"),
	);
	assert.throws(() => subscriberAddresses("a@example.com", "SNS"));
	assert.throws(() => subscriberAddresses("arn:aws:sns:bad-arn", "SNS"));
	assert.throws(() => subscriberAddresses("not-an-email", "EMAIL"));
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
	assert.throws(() => expression({}, "threshold"));
	assert.throws(() =>
		expression({ Or: [threshold("100")], And: [threshold("50")] }, "threshold"),
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
	assert.throws(() => expression({ And: [threshold("100")] }, "monitor"));
});

test("custom and dimensional monitor specifications have different boundaries", () => {
	assert.doesNotThrow(() =>
		monitorSpecification({ Tags: { Key: "team" } }, "DIMENSIONAL", "TAG"),
	);
	assert.deepEqual(
		monitorSpecification(
			{ CostCategories: { Key: "Department" } },
			"DIMENSIONAL",
			"COST_CATEGORY",
		),
		{ CostCategories: { Key: "Department" } },
	);
	assert.throws(() =>
		monitorSpecification(
			{ Tags: { Key: "team", Values: ["a"] } },
			"DIMENSIONAL",
			"TAG",
		),
	);
	assert.throws(() =>
		monitorSpecification(
			{ CostCategories: { Key: "Department", Values: ["a"] } },
			"DIMENSIONAL",
			"COST_CATEGORY",
		),
	);
	assert.throws(() =>
		monitorSpecification(
			{ Tags: { Key: "team" } },
			"DIMENSIONAL",
			"COST_CATEGORY",
		),
	);
	assert.throws(() =>
		monitorSpecification(
			{ Tags: { Key: "team", MatchOptions: ["EQUALS"] } },
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
	assert.doesNotThrow(() =>
		monitorSpecification(
			{ Tags: { Key: "environment", Values: ["prod", "stage"] } },
			"CUSTOM",
			"",
		),
	);
	assert.doesNotThrow(() =>
		monitorSpecification(
			{ CostCategories: { Key: "team", Values: ["platform"] } },
			"CUSTOM",
			"",
		),
	);
	assert.throws(() =>
		monitorSpecification({ Tags: { Key: "env", Values: [] } }, "CUSTOM", ""),
	);
	assert.throws(() =>
		monitorSpecification(
			{ Tags: { Key: "env", Values: Array(11).fill("val") } },
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
	assert.throws(() => jsonArray("{bad json", "Rules"));
	assert.throws(() => jsonArray("123", "Rules"));
	assert.throws(() => jsonArray('"not an array"', "Rules"));
	assert.throws(() => jsonArray("[1, 2]", "Rules"));
	assert.throws(() => jsonArray("[[]]", "Rules"));
	assert.throws(() => jsonArray("[{}, {}]", "Rules", 1, 1));
	assert.throws(() => jsonArray("{}", "Rules", 1));
	assert.throws(() => jsonArray("[]", "Rules", 1));
	assert.throws(() => jsonArray("[null]", "Rules", 1));
	assert.deepEqual(jsonArray("[]", "Rules"), []);
	assert.deepEqual(jsonArray('[{"Key":"team","Values":["eng"]}]', "Rules"), [
		{ Key: "team", Values: ["eng"] },
	]);
	assert.equal(categoryName("Compute team"), "Compute team");
	assert.equal(categoryName("Équipe_Projet-1"), "Équipe_Projet-1");
	assert.throws(() => categoryName(""));
	assert.throws(() => categoryName(" leading"));
	assert.throws(() => categoryName("trailing "));
	assert.throws(() => categoryName("x".repeat(51)));
	assert.throws(() => categoryName("team@domain"));
	assert.throws(() => categoryName("team#cost"));
	assert.throws(() =>
		expression({ Dimensions: { Key: "SERVICE", Values: ["EC2"] } }, "category"),
	);
	assert.doesNotThrow(() =>
		expression(
			{
				And: [
					{
						Not: {
							Dimensions: { Key: "REGION", Values: ["us-east-1"] },
						},
					},
					{
						Tags: {
							Key: "env",
							Values: ["prod"],
							MatchOptions: ["EQUALS"],
						},
					},
				],
			},
			"category",
		),
	);
	for (const dim of [
		"LINKED_ACCOUNT",
		"SERVICE_CODE",
		"RECORD_TYPE",
		"LINKED_ACCOUNT_NAME",
		"REGION",
		"USAGE_TYPE",
	]) {
		assert.doesNotThrow(() =>
			expression({ Dimensions: { Key: dim, Values: ["test"] } }, "category"),
		);
	}
	assert.throws(() =>
		expression({ Dimensions: { Key: "k".repeat(1025) } }, "category"),
	);
	assert.throws(() =>
		expression(
			{ Dimensions: { Key: "REGION", Values: ["v".repeat(1025)] } },
			"category",
		),
	);
	assert.throws(() =>
		expression(
			{
				Dimensions: {
					Key: "REGION",
					Values: ["us-east-1"],
					MatchOptions: ["NOT_AN_OPTION"],
				},
			},
			"category",
		),
	);
});
