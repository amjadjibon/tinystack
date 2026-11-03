import { api } from "./api";
import type { MQScope } from "./mqHelpers";
export const mqCall = <T = Record<string, unknown>>(
	s: MQScope,
	op: string,
	input: Record<string, unknown> = {},
): Promise<T> => api.action("mq", op, s.account, input, s.region);
export async function mqList<T>(
	s: MQScope,
	op: string,
	key: string,
	input: Record<string, unknown> = {},
) {
	const rows: T[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const out = await mqCall<Record<string, unknown>>(s, op, {
			...input,
			MaxResults: 100,
			...(token ? { NextToken: token } : {}),
		});
		rows.push(...((out[key] ?? []) as T[]));
		token = out.NextToken as string | undefined;
		if (token && seen.has(token))
			throw new Error("The server returned a repeated page token.");
		if (token) seen.add(token);
	} while (token);
	return rows;
}
