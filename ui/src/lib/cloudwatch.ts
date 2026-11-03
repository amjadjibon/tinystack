import { api } from "./api";
import { type ActionStep, integer, stringMap } from "./consoleActions";
import { tagList } from "./tags.ts";
export type WatchScope = {
	account: string;
	region: string;
	alarm?: string;
	dashboard?: string;
};
export type Metric = {
	Namespace: string;
	MetricName: string;
	Dimensions?: { Name: string; Value: string }[];
};
export type MetricQuery = {
	Id: string;
	Label?: string;
	ReturnData?: boolean;
	Expression?: string;
	MetricStat?: { Metric: Metric; Period: number; Stat: string; Unit?: string };
};
export type MetricResult = {
	Id: string;
	Label?: string;
	Timestamps?: number[];
	Values?: number[];
	StatusCode?: string;
	Messages?: { Code?: string; Value?: string }[];
};
export type Alarm = Partial<Metric> & {
	AlarmName: string;
	AlarmArn?: string;
	AlarmDescription?: string;
	AlarmRule?: string;
	ActionsEnabled?: boolean;
	AlarmActions?: string[];
	OKActions?: string[];
	InsufficientDataActions?: string[];
	StateValue?: string;
	StateReason?: string;
	StateReasonData?: string;
	StateUpdatedTimestamp?: number;
	Period?: number;
	Statistic?: string;
	ExtendedStatistic?: string;
	Unit?: string;
	EvaluationPeriods?: number;
	DatapointsToAlarm?: number;
	Threshold?: number;
	ComparisonOperator?: string;
	TreatMissingData?: string;
	EvaluateLowSampleCountPercentile?: string;
	Metrics?: MetricQuery[];
	Type: "MetricAlarm" | "CompositeAlarm";
};
export type HistoryItem = {
	Timestamp?: number;
	HistoryItemType?: string;
	HistorySummary?: string;
	HistoryData?: string;
};
export const alarmTypes = ["MetricAlarm", "CompositeAlarm"];
export const states = ["ALARM", "OK", "INSUFFICIENT_DATA"];
export const comparisons = [
	"GreaterThanThreshold",
	"GreaterThanOrEqualToThreshold",
	"LessThanThreshold",
	"LessThanOrEqualToThreshold",
];
export const statistics = [
	"Average",
	"Sum",
	"Minimum",
	"Maximum",
	"SampleCount",
	"p90",
	"p95",
	"p99",
];
export function watchCall<T = Record<string, unknown>>(
	scope: WatchScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action(
		"cloudwatch",
		operation,
		scope.account,
		input,
		scope.region,
	);
}
export function cloudwatchHref(scope: WatchScope, section = "alarms") {
	const p = new URLSearchParams({
		account: scope.account,
		region: scope.region,
		section,
	});
	if (scope.alarm) p.set("alarm", scope.alarm);
	if (scope.dashboard) p.set("dashboard", scope.dashboard);
	return `/services/cloudwatch?${p}`;
}
export function metricKey(m: Metric) {
	return JSON.stringify([
		m.Namespace,
		m.MetricName,
		[...(m.Dimensions ?? [])].sort((a, b) => a.Name.localeCompare(b.Name)),
	]);
}
export function dimensionText(m: Partial<Metric>) {
	return (m.Dimensions ?? []).map((d) => `${d.Name}=${d.Value}`).join(", ");
}
export function dimensions(text: string) {
	return Object.entries(stringMap(text, "Dimensions")).map(([Name, Value]) => ({
		Name,
		Value,
	}));
}
export function finite(text: string, label: string) {
	if (!text.trim() || !Number.isFinite(Number(text)))
		throw new Error(`${label} must be a finite number.`);
	return Number(text);
}
export function periodValue(text: string, alarm = false) {
	const p = integer(text, "Period", 1, 86400);
	if (!(alarm ? [10, 20, 30] : [1, 5, 10, 30]).includes(p) && p % 60 !== 0)
		throw new Error(
			alarm
				? "Period must be 10, 20, 30, or a multiple of 60 seconds."
				: "Period must be 1, 5, 10, 30, or a multiple of 60 seconds.",
		);
	return p;
}
export function timeRange(start: string, end: string) {
	const s = Date.parse(start) / 1000,
		e = Date.parse(end) / 1000;
	if (!Number.isFinite(s) || !Number.isFinite(e) || s >= e)
		throw new Error("Enter valid start and end times, with start before end.");
	return { StartTime: s, EndTime: e };
}
export function lastHour() {
	const now = Date.now();
	return [
		new Date(now - 3600000).toISOString(),
		new Date(now).toISOString(),
	] as const;
}
export function timestamp(t?: number) {
	return t === undefined ? "—" : new Date(t * 1000).toISOString();
}
export async function listMetrics(scope: WatchScope): Promise<Metric[]> {
	const out: Metric[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const page = await watchCall<{ Metrics?: Metric[]; NextToken?: string }>(
			scope,
			"ListMetrics",
			token ? { NextToken: token } : {},
		);
		out.push(...(page.Metrics ?? []));
		token = page.NextToken;
		if (token && seen.has(token))
			throw new Error("Metric pagination did not advance. Refresh to retry.");
		if (token) seen.add(token);
	} while (token);
	return out;
}
export async function describeAlarm(scope: WatchScope): Promise<Alarm> {
	const page = await watchCall<{
		MetricAlarms?: Omit<Alarm, "Type">[];
		CompositeAlarms?: Omit<Alarm, "Type">[];
	}>(scope, "DescribeAlarms", {
		AlarmNames: [scope.alarm],
		AlarmTypes: alarmTypes,
	});
	const a = page.MetricAlarms?.[0],
		c = page.CompositeAlarms?.[0];
	if (!a && !c)
		throw new Error("Alarm not found in the selected account and Region.");
	return { ...(a ?? c), Type: a ? "MetricAlarm" : "CompositeAlarm" } as Alarm;
}
export function watchTagPlan(
	scope: WatchScope,
	arn: string,
	tags: Record<string, string>,
	removed: string[],
): ActionStep[] {
	return [
		...(removed.length
			? [
					{
						label: "Remove tags",
						run: () =>
							watchCall(scope, "UntagResource", {
								ResourceARN: arn,
								TagKeys: removed,
							}),
					},
				]
			: []),
		...(Object.keys(tags).length
			? [
					{
						label: "Save tags",
						run: () =>
							watchCall(scope, "TagResource", {
								ResourceARN: arn,
								Tags: tagList(tags),
							}),
					},
				]
			: []),
	];
}
export function alarmInput(a: Alarm): Record<string, unknown> {
	const keys = [
		"AlarmName",
		"AlarmDescription",
		"ActionsEnabled",
		"OKActions",
		"AlarmActions",
		"InsufficientDataActions",
		...(a.Type === "CompositeAlarm"
			? ["AlarmRule"]
			: [
					"Namespace",
					"MetricName",
					"Dimensions",
					"Period",
					"Statistic",
					"ExtendedStatistic",
					"Unit",
					"EvaluationPeriods",
					"DatapointsToAlarm",
					"Threshold",
					"ComparisonOperator",
					"TreatMissingData",
					"EvaluateLowSampleCountPercentile",
					"Metrics",
				]),
	];
	return Object.fromEntries(
		keys
			.filter((k) => a[k as keyof Alarm] !== undefined)
			.map((k) => [k, a[k as keyof Alarm]]),
	);
}
export type DashboardWidget = {
	type?: string;
	properties?: Record<string, unknown>;
};
export function dashboardQueries(
	properties: Record<string, unknown>,
	region: string,
): { queries: MetricQuery[]; region: string } {
	if (properties.accountId !== undefined)
		throw new Error("Cross-account metric previews are unavailable.");
	const lines = properties.metrics;
	if (!Array.isArray(lines) || !lines.length)
		throw new Error("This widget has no metric queries.");
	if (lines.length > 10)
		throw new Error(
			"Metric previews support up to ten queries per widget. Edit the source to narrow this widget.",
		);
	let previous: string[] = [];
	const queries: MetricQuery[] = lines.map((line: unknown, i) => {
		if (!Array.isArray(line) || !line.length)
			throw new Error("Metric definitions must be arrays.");
		const last = line.at(-1);
		const options =
			last && typeof last === "object" && !Array.isArray(last)
				? (last as Record<string, unknown>)
				: {};
		if (options.accountId !== undefined)
			throw new Error("Cross-account metric previews are unavailable.");
		if (
			options.region !== undefined &&
			options.region !== (properties.region ?? region)
		)
			throw new Error("Use one Region per metric widget.");
		const Id = String(options.id ?? `m${i}`),
			Label = typeof options.label === "string" ? options.label : undefined,
			ReturnData = options.visible !== false;
		if (typeof options.expression === "string")
			return { Id, Label, ReturnData, Expression: options.expression };
		const tokens = (
			Object.keys(options).length || last === options ? line.slice(0, -1) : line
		).map((v, j) => (v === "." ? previous[j] : v));
		if (
			tokens.length < 2 ||
			tokens.length % 2 ||
			tokens.some((v) => typeof v !== "string")
		)
			throw new Error(
				"Use namespace, metric name, and dimension name/value pairs.",
			);
		previous = tokens as string[];
		const Dimensions: { Name: string; Value: string }[] = [];
		for (let j = 2; j < tokens.length; j += 2)
			Dimensions.push({
				Name: tokens[j] as string,
				Value: tokens[j + 1] as string,
			});
		return {
			Id,
			Label,
			ReturnData,
			MetricStat: {
				Metric: {
					Namespace: tokens[0] as string,
					MetricName: tokens[1] as string,
					Dimensions,
				},
				Period: periodValue(String(options.period ?? properties.period ?? 300)),
				Stat: String(options.stat ?? properties.stat ?? "Average"),
				...(typeof options.unit === "string" ? { Unit: options.unit } : {}),
			},
		};
	});
	const selectedRegion = String(properties.region ?? region);
	if (!/^[a-z]{2}(?:-[a-z]+)+-\d+$/.test(selectedRegion))
		throw new Error("Widget Region is invalid.");
	return { queries, region: selectedRegion };
}
