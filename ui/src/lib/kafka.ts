import { api } from "./api";
import type { KafkaScope } from "./kafkaHelpers";
export const kafkaCall = <T = Record<string, unknown>>(
	s: KafkaScope,
	op: string,
	input: Record<string, unknown> = {},
): Promise<T> => api.action("kafka", op, s.account, input, s.region);
export async function kafkaList<T>(
	s: KafkaScope,
	op: string,
	key: string,
	input: Record<string, unknown> = {},
) {
	const rows: T[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const out = await kafkaCall<Record<string, unknown>>(s, op, {
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
