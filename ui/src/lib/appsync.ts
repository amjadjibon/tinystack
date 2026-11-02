import { api } from "./api";
import type { RecordValue } from "./appsyncHelpers";
export type AppSyncScope = { account: string; region: string };
export function appsyncCall<T = RecordValue>(
	scope: AppSyncScope,
	operation: string,
	input: RecordValue = {},
): Promise<T> {
	return api.action("appsync", operation, scope.account, input, scope.region);
}
export async function listAppSync<T extends RecordValue>(
	scope: AppSyncScope,
	operation: string,
	member: string,
	input: RecordValue = {},
): Promise<T[]> {
	const items: T[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const out = await appsyncCall<Record<string, unknown>>(scope, operation, {
			...input,
			MaxResults: 25,
			...(token ? { NextToken: token } : {}),
		});
		items.push(...((out[member] ?? []) as T[]));
		token = out.NextToken as string | undefined;
		if (token && seen.has(token))
			throw new Error("The server returned a repeated page token.");
		if (token) seen.add(token);
	} while (token);
	return items;
}
