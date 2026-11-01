import { api } from "./api";
import type { ActionStep } from "./consoleActions";
import { parameterMetadata } from "./ssmHelpers";

export type SSMScope = { account: string; region: string; parameter?: string };
export type Parameter = {
	Name: string;
	ARN?: string;
	Type: string;
	Tier?: string;
	Version: number;
	Description?: string;
	KeyId?: string;
	AllowedPattern?: string;
	DataType?: string;
	LastModifiedDate?: number;
	Labels?: string[];
};
export function ssmHref(s: SSMScope) {
	const query = new URLSearchParams({ account: s.account, region: s.region });
	if (s.parameter) query.set("parameter", s.parameter);
	return `/services/ssm?${query}`;
}
export function ssmCall<T = Record<string, unknown>>(
	s: SSMScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("ssm", operation, s.account, input, s.region);
}
// All paginated caches contain metadata only, including history responses.
export async function ssmPages(
	s: SSMScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<Parameter[]> {
	const items: Parameter[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const out = await ssmCall<{
			Parameters?: (Parameter & { Value?: string })[];
			NextToken?: string;
		}>(s, operation, {
			...input,
			MaxResults: 50,
			...(token ? { NextToken: token } : {}),
		});
		items.push(...(out.Parameters ?? []).map(parameterMetadata));
		token = out.NextToken;
		if (token && seen.has(token))
			throw new Error("The server returned a repeated page token.");
		if (token) seen.add(token);
	} while (token);
	return items;
}
export function ssmTagPlan(
	s: SSMScope,
	tags: Record<string, string>,
	removed: string[],
): ActionStep[] {
	const input = { ResourceType: "Parameter", ResourceId: s.parameter };
	return [
		...(removed.length
			? [
					{
						label: "Remove tags",
						run: () =>
							ssmCall(s, "RemoveTagsFromResource", {
								...input,
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
							ssmCall(s, "AddTagsToResource", {
								...input,
								Tags: Object.entries(tags).map(([Key, Value]) => ({
									Key,
									Value,
								})),
							}),
					},
				]
			: []),
	];
}
