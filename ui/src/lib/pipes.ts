import { api } from "./api";
import type { Pipe } from "./pipesHelpers";
export type PipesScope = { account: string; region: string };
export function pipesHref(s: PipesScope) {
	return `/services/pipes?${new URLSearchParams(s)}`;
}
export function pipesCall<T = Record<string, unknown>>(
	s: PipesScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("pipes", operation, s.account, input, s.region);
}
export async function listPipes(s: PipesScope): Promise<Pipe[]> {
	const items: Pipe[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const out = await pipesCall<{ Pipes?: Pipe[]; NextToken?: string }>(
			s,
			"ListPipes",
			{ Limit: 100, ...(token ? { NextToken: token } : {}) },
		);
		items.push(...(out.Pipes ?? []));
		token = out.NextToken;
		if (token && seen.has(token))
			throw new Error("The server returned a repeated page token.");
		if (token) seen.add(token);
	} while (token);
	return items;
}
