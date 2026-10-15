import assert from "node:assert/strict";
import test from "node:test";
import {
	costPercentage,
	costPreset,
	decimalUnits,
	sumCosts,
	usageRows,
} from "../src/lib/costUsage.ts";

test("decimal units parse exact scale and reject malformed input", () => {
	assert.equal(decimalUnits("0"), 0n);
	assert.equal(decimalUnits("-0"), 0n);
	assert.equal(decimalUnits("1"), 1000000000000n);
	assert.equal(decimalUnits("-1"), -1000000000000n);
	assert.equal(decimalUnits("0.000000000001"), 1n);
	assert.equal(decimalUnits("-0.000000000001"), -1n);
	assert.equal(decimalUnits("12.3456789012"), 12345678901200n);
	assert.equal(decimalUnits("-1.5"), -1500000000000n);
	for (const bad of [
		"",
		"   ",
		"abc",
		".5",
		"1.",
		"1.1234567890123",
		"--1",
		"+1",
		"1e5",
	]) {
		assert.throws(() => decimalUnits(bad));
	}
});

test("cost totals retain precision, credits and large amounts", () => {
	assert.equal(sumCosts([]), "0");
	assert.equal(sumCosts(["10", "25"]), "35");
	assert.equal(sumCosts(["-10", "-25"]), "-35");
	assert.equal(sumCosts(["-10.5", "5"]), "-5.5");
	assert.equal(sumCosts(["1.5", "-1.5"]), "0");
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
	assert.deepEqual(costPreset("lastmonth", new Date("2026-01-15T00:00:00Z")), {
		Start: "2025-12-01",
		End: "2026-01-01",
	});
	assert.deepEqual(costPreset("7days", now), {
		Start: "2024-02-24",
		End: "2024-03-02",
	});
	assert.deepEqual(costPreset("3months", new Date("2026-01-31T00:00:00Z")), {
		Start: "2025-11-01",
		End: "2026-02-01",
	});
	const defaultPreset = costPreset("month");
	assert.match(defaultPreset.Start, /^\d{4}-\d{2}-01$/);
	assert.match(defaultPreset.End, /^\d{4}-\d{2}-\d{2}$/);
});

test("usage rows preserve amounts and estimated status", () => {
	assert.deepEqual(usageRows([]), []);
	assert.deepEqual(
		usageRows([
			{
				TimePeriod: { Start: "2026-01-01", End: "2026-01-02" },
				Total: { UnblendedCost: { Amount: "42.50" } },
			},
		]),
		[
			{
				start: "2026-01-01",
				end: "2026-01-02",
				estimated: false,
				amounts: { Cost: "42.50" },
			},
		],
	);
	assert.deepEqual(
		usageRows([
			{
				TimePeriod: { Start: "2026-10-01", End: "2026-10-02" },
				Estimated: true,
				Groups: [
					{ Keys: ["team$a"], Metrics: { UnblendedCost: { Amount: "-0.1" } } },
					{
						Keys: ["AmazonEC2", "us-east-1"],
						Metrics: { UnblendedCost: { Amount: "15.00" } },
					},
				],
			},
		]),
		[
			{
				start: "2026-10-01",
				end: "2026-10-02",
				estimated: true,
				amounts: { team$a: "-0.1", "AmazonEC2 / us-east-1": "15.00" },
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
	assert.equal(costPercentage("10", "-5"), undefined);
	assert.equal(costPercentage("-10", "-5"), undefined);
	assert.equal(costPercentage("0", "100"), "0.00");
	assert.equal(costPercentage("-0.0001", "100"), "0.00");
	assert.equal(costPercentage("20", "30"), "66.67");
});
