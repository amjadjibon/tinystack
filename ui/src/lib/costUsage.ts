// Costs have at most twelve fractional digits. Keep all table arithmetic exact.
const scale = 10n ** 12n;
export function decimalUnits(value: string): bigint {
	if (!/^-?\d+(\.\d{1,12})?$/.test(value))
		throw new Error(`Invalid cost: ${value}`);
	const [whole, fraction = ""] = value.replace(/^-/, "").split(".");
	return (
		(BigInt(whole) * scale + BigInt(fraction.padEnd(12, "0"))) *
		(value.startsWith("-") ? -1n : 1n)
	);
}
export function sumCosts(values: string[]): string {
	const total = values.reduce((sum, value) => sum + decimalUnits(value), 0n);
	const magnitude = total < 0n ? -total : total;
	const fraction = String(magnitude % scale)
		.padStart(12, "0")
		.replace(/0+$/, "");
	return `${total < 0n ? "-" : ""}${magnitude / scale}${fraction ? `.${fraction}` : ""}`;
}
export function costPreset(preset: string, now = new Date()) {
	const day = new Date(
		Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()),
	);
	const end = new Date(day);
	end.setUTCDate(end.getUTCDate() + 1);
	const start = new Date(day);
	start.setUTCDate(1);
	if (preset === "7days") start.setUTCDate(day.getUTCDate() - 6);
	if (preset === "lastmonth") {
		end.setTime(start.getTime());
		start.setUTCMonth(start.getUTCMonth() - 1);
	}
	if (preset === "3months") start.setUTCMonth(start.getUTCMonth() - 2);
	return {
		Start: start.toISOString().slice(0, 10),
		End: end.toISOString().slice(0, 10),
	};
}
export type CostPeriod = {
	TimePeriod: { Start: string; End: string };
	Estimated?: boolean;
	Total?: { UnblendedCost: { Amount: string } };
	Groups?: { Keys: string[]; Metrics: { UnblendedCost: { Amount: string } } }[];
};
export function usageRows(periods: CostPeriod[]) {
	return periods.map((p) => ({
		start: p.TimePeriod.Start,
		end: p.TimePeriod.End,
		estimated: !!p.Estimated,
		amounts: Object.fromEntries(
			p.Groups?.map((g) => [
				g.Keys.join(" / "),
				g.Metrics.UnblendedCost.Amount,
			]) ?? (p.Total ? [["Cost", p.Total.UnblendedCost.Amount]] : []),
		),
	}));
}

// Return a percentage rounded to two places without converting money to Number.
export function costPercentage(
	actual: string,
	limit: string,
): string | undefined {
	const digits = Math.max(
		actual.split(".")[1]?.length ?? 0,
		limit.split(".")[1]?.length ?? 0,
	);
	const units = (value: string) => {
		const [whole, fraction = ""] = value.replace(/^-/, "").split(".");
		return (
			BigInt(whole + fraction.padEnd(digits, "0")) *
			(value.startsWith("-") ? -1n : 1n)
		);
	};
	const denominator = units(limit);
	if (denominator <= 0n) return undefined;
	const numerator = units(actual);
	const magnitude = numerator < 0n ? -numerator : numerator;
	const rounded = (magnitude * 10000n + denominator / 2n) / denominator;
	return `${numerator < 0n && rounded !== 0n ? "-" : ""}${rounded / 100n}.${String(rounded % 100n).padStart(2, "0")}`;
}
