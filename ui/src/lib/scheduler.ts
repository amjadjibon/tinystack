import { api } from "./api";

export type SchedulerScope = { account: string; region: string };
export type ScheduleGroup = {
	Name: string;
	Arn: string;
	State?: string;
	CreationDate?: number;
	LastModificationDate?: number;
};
export type ScheduleSummary = {
	Name: string;
	GroupName: string;
	Arn: string;
	State?: string;
	CreationDate?: number;
	LastModificationDate?: number;
	Target?: { Arn?: string };
};
export function schedulerHref(s: SchedulerScope) {
	return `/services/scheduler?${new URLSearchParams({ account: s.account, region: s.region })}`;
}
export function schedulerCall<T = Record<string, unknown>>(
	s: SchedulerScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("scheduler", operation, s.account, input, s.region);
}
export async function schedulerPages<T>(
	s: SchedulerScope,
	operation: string,
	key: string,
	input: Record<string, unknown> = {},
): Promise<T[]> {
	const items: T[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const out = await schedulerCall<Record<string, unknown>>(s, operation, {
			...input,
			MaxResults: 100,
			...(token ? { NextToken: token } : {}),
		});
		items.push(...((out[key] as T[] | undefined) ?? []));
		token = out.NextToken as string | undefined;
		if (token && seen.has(token))
			throw new Error("The server returned a repeated page token.");
		if (token) seen.add(token);
	} while (token);
	return items;
}
export const listGroups = (s: SchedulerScope) =>
	schedulerPages<ScheduleGroup>(s, "ListScheduleGroups", "ScheduleGroups");
export const listSchedules = (s: SchedulerScope, group?: string) =>
	schedulerPages<ScheduleSummary>(
		s,
		"ListSchedules",
		"Schedules",
		group ? { GroupName: group } : {},
	);
