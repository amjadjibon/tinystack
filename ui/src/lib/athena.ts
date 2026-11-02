import { api } from "./api";
import { batches, type NamedQuery, type QueryExecution } from "./athenaHelpers";
export type AthenaScope = { account: string; region: string };
export function athenaHref(s: AthenaScope) {
	return `/services/athena?${new URLSearchParams(s)}`;
}
export function athenaCall<T = Record<string, unknown>>(
	s: AthenaScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("athena", operation, s.account, input, s.region);
}
export async function athenaPages<T>(
	s: AthenaScope,
	operation: string,
	key: string,
	input: Record<string, unknown> = {},
): Promise<T[]> {
	const items: T[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const out = await athenaCall<Record<string, unknown>>(s, operation, {
			...input,
			MaxResults: 50,
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
export async function queryHistory(s: AthenaScope, workgroup: string) {
	const ids = await athenaPages<string>(
		s,
		"ListQueryExecutions",
		"QueryExecutionIds",
		{ WorkGroup: workgroup },
	);
	const pages = await Promise.all(
		batches(ids).map((QueryExecutionIds) =>
			athenaCall<{
				QueryExecutions?: QueryExecution[];
				UnprocessedQueryExecutionIds?: { ErrorMessage?: string }[];
			}>(s, "BatchGetQueryExecution", { QueryExecutionIds }),
		),
	);
	for (const page of pages)
		if (page.UnprocessedQueryExecutionIds?.length)
			throw new Error(
				page.UnprocessedQueryExecutionIds[0].ErrorMessage ??
					"Could not load query history.",
			);
	return pages.flatMap((p) => p.QueryExecutions ?? []);
}
export async function namedQueries(s: AthenaScope, workgroup: string) {
	const ids = await athenaPages<string>(
		s,
		"ListNamedQueries",
		"NamedQueryIds",
		{ WorkGroup: workgroup },
	);
	const pages = await Promise.all(
		batches(ids).map((NamedQueryIds) =>
			athenaCall<{
				NamedQueries?: NamedQuery[];
				UnprocessedNamedQueryIds?: { ErrorMessage?: string }[];
			}>(s, "BatchGetNamedQuery", { NamedQueryIds }),
		),
	);
	for (const page of pages)
		if (page.UnprocessedNamedQueryIds?.length)
			throw new Error(
				page.UnprocessedNamedQueryIds[0].ErrorMessage ??
					"Could not load saved queries.",
			);
	return pages.flatMap((p) => p.NamedQueries ?? []);
}
