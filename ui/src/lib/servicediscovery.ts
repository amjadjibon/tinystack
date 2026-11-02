import { api } from "./api";
export type CloudMapScope = { account: string; region: string };
export function cloudMapHref(s: CloudMapScope) {
	return `/services/servicediscovery?${new URLSearchParams(s)}`;
}
export function cloudMapCall<T = Record<string, unknown>>(
	s: CloudMapScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("servicediscovery", operation, s.account, input, s.region);
}
export async function cloudMapList<T>(
	s: CloudMapScope,
	operation: string,
	member: string,
	input: Record<string, unknown> = {},
): Promise<T[]> {
	const items: T[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const out = await cloudMapCall<Record<string, unknown>>(s, operation, {
			...input,
			MaxResults: 100,
			...(token ? { NextToken: token } : {}),
		});
		items.push(...((out[member] as T[]) ?? []));
		token = out.NextToken as string | undefined;
		if (token && seen.has(token))
			throw new Error("The server returned a repeated page token.");
		if (token) seen.add(token);
	} while (token);
	return items;
}
