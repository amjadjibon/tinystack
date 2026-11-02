import { api } from "./api";
import {
	type GlueContext,
	type GlueKind,
	type GlueRecord,
	glueLists,
} from "./glueHelpers";
export type GlueScope = { account: string; region: string };
export function glueCall<T = Record<string, unknown>>(
	s: GlueScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("glue", operation, s.account, input, s.region);
}
export async function gluePages<T>(
	fetch: (input: Record<string, unknown>) => Promise<Record<string, unknown>>,
	key: string,
	input: Record<string, unknown> = {},
): Promise<T[]> {
	const items: T[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const out = await fetch({
			...input,
			MaxResults: 25,
			...(token ? { NextToken: token } : {}),
		});
		items.push(...((out[key] ?? []) as T[]));
		token = out.NextToken as string | undefined;
		if (token && seen.has(token))
			throw new Error("The server returned a repeated page token.");
		if (token) seen.add(token);
	} while (token);
	return items;
}
export async function listGlue(
	s: GlueScope,
	kind: GlueKind,
	context: GlueContext,
): Promise<GlueRecord[]> {
	const [operation, key] = glueLists[kind];
	if (kind === "Workflow") {
		const names = await gluePages<string>(
			(input) => glueCall(s, operation, input),
			key,
		);
		return Promise.all(
			names.map(
				async (Name) =>
					(await glueCall<{ Workflow: GlueRecord }>(s, "GetWorkflow", { Name }))
						.Workflow,
			),
		);
	}
	return gluePages<GlueRecord>(
		(input) => glueCall(s, operation, input),
		key,
		context,
	);
}

export function glueHref(s: GlueScope) {
	return `/services/glue?${new URLSearchParams(s)}`;
}
