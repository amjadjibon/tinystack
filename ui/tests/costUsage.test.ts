import assert from "node:assert/strict";
import test from "node:test";
import {
	costPercentage,
	costPreset,
	sumCosts,
	usageRows,
} from "../src/lib/costUsage.ts";

test("cost totals retain precision, credits and large amounts", () => {
	assert.equal(sumCosts(["0.1", "0.2", "-0.05"]), "0.25");
	assert.equal(
		sumCosts(["999999999999999999999999999999.999999999999", "0.000000000001"]),
		"1000000000000000000000000000000",
	);
	assert.equal(sumCosts(["-1.000000000001", "1"]), "-0.000000000001");
	assert.equal(sumCosts(["-0", "1.20", "-1.2"]), "0");
	assert.throws(() => sumCosts(["1e3"]));
});
test("UTC presets include today and cross month, leap year and year boundaries", () => {
	const now = new Date("2024-03-01T23:59:00Z");
	assert.deepEqual(costPreset("month", now), {
		Start: "2024-03-01",
		End: "2024-03-02",
	});
	assert.deepEqual(costPreset("lastmonth", now), {
		Start: "2024-02-01",
		End: "2024-03-01",
	});
	assert.deepEqual(costPreset("7days", now), {
		Start: "2024-02-24",
		End: "2024-03-02",
	});
	assert.deepEqual(costPreset("3months", new Date("2026-01-31T00:00:00Z")), {
		Start: "2025-11-01",
		End: "2026-02-01",
	});
});
test("usage rows preserve amounts and estimated status", () => {
	assert.deepEqual(
		usageRows([
			{
				TimePeriod: { Start: "2026-10-01", End: "2026-10-02" },
				Estimated: true,
				Groups: [
					{ Keys: ["team$a"], Metrics: { UnblendedCost: { Amount: "-0.1" } } },
				],
			},
		]),
		[
			{
				start: "2026-10-01",
				end: "2026-10-02",
				estimated: true,
				amounts: { team$a: "-0.1" },
			},
		],
	);
});

test("empty grouped periods do not invent an ungrouped cost series", () => {
	assert.deepEqual(
		usageRows([{ TimePeriod: { Start: "2026-08-01", End: "2026-09-01" } }])[0]
			.amounts,
		{},
	);
});

test("budget percentages use decimal ratios including sub-cent limits", () => {
	assert.equal(costPercentage("30.1", "50"), "60.20");
	assert.equal(costPercentage("1", "3"), "33.33");
	assert.equal(
		costPercentage("0.000000000001", "0.00000000000001"),
		"10000.00",
	);
	assert.equal(costPercentage("-2.345", "10"), "-23.45");
	assert.equal(costPercentage("1", "0"), undefined);
});
