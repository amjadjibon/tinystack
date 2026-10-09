import { api, type Resource } from "./api";
export type LogsScope = {
	account: string;
	region: string;
	group?: string;
	stream?: string;
};
export type LogGroup = {
	LogGroupName?: string;
	Arn?: string;
	CreationTime?: number;
	RetentionInDays?: number;
	StoredBytes?: number;
	LogGroupClass?: string;
};
export type LogStream = {
	LogStreamName?: string;
	CreationTime?: number;
	FirstEventTimestamp?: number;
	LastEventTimestamp?: number;
	LastIngestionTime?: number;
	Arn?: string;
};
export type LogEvent = {
	Timestamp?: number;
	Message?: string;
	LogStreamName?: string;
	IngestionTime?: number;
	EventId?: string;
};
export const retentionDays = [
	1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827,
	2192, 2557, 2922, 3288, 3653,
];
export function logsHref(scope: LogsScope, section = "groups") {
	const p = new URLSearchParams({
		account: scope.account,
		region: scope.region,
	});
	if (scope.group) p.set("group", scope.group);
	if (scope.stream) p.set("stream", scope.stream);
	if (section !== "groups") p.set("section", section);
	return `/services/logs?${p}`;
}
export function logsCall<T = Record<string, unknown>>(
	scope: LogsScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("logs", operation, scope.account, input, scope.region);
}
export function groupCall<T = Record<string, unknown>>(
	scope: LogsScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return logsCall(scope, operation, { LogGroupName: scope.group, ...input });
}
export function logGroup(resource?: Resource): LogGroup {
	// Inventory uses the service's wire JSON names; console actions use SDK names.
	const raw = resource?.details?.LogGroup as
		| {
				logGroupName?: string;
				arn?: string;
				creationTime?: number;
				retentionInDays?: number;
				storedBytes?: number;
				logGroupClass?: string;
		  }
		| undefined;
	return raw
		? {
				LogGroupName: raw.logGroupName,
				Arn: raw.arn,
				CreationTime: raw.creationTime,
				RetentionInDays: raw.retentionInDays,
				StoredBytes: raw.storedBytes,
				LogGroupClass: raw.logGroupClass,
			}
		: {};
}
export function logsArn(scope: LogsScope) {
	return `arn:aws:logs:${scope.region}:${scope.account}:log-group:${scope.group}`;
}
export function logTime(text: string, label: string) {
	if (!text.trim()) return undefined;
	const value = Date.parse(text);
	if (!Number.isSafeInteger(value) || value < 0)
		throw new Error(`${label} must be a valid date and time after 1970.`);
	return value;
}

export async function logStreams(scope: LogsScope): Promise<LogStream[]> {
	const streams: LogStream[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const page = await groupCall<{
			LogStreams?: LogStream[];
			NextToken?: string;
		}>(scope, "DescribeLogStreams", {
			Limit: 50,
			...(token ? { NextToken: token } : {}),
		});
		streams.push(...(page.LogStreams ?? []));
		token = page.NextToken;
		if (token && seen.has(token))
			throw new Error(
				"The log stream continuation token did not advance. Refresh to retry.",
			);
		if (token) seen.add(token);
	} while (token);
	return streams;
}
