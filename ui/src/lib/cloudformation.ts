import { api } from "./api";
import type { ChangeSetDetail, TemplateSummary } from "./cloudformationHelpers";

export type CfnScope = { account: string; region: string; stack?: string };

export function cfnHref(s: CfnScope, changeSet?: string) {
	const p = new URLSearchParams({ account: s.account, region: s.region });
	if (s.stack) p.set("stack", s.stack);
	if (changeSet) p.set("changeset", changeSet);
	return `/services/cloudformation?${p}`;
}

export function cfnCall<T = Record<string, unknown>>(
	s: CfnScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("cloudformation", operation, s.account, input, s.region);
}

// cfnPages follows NextToken. limit caps the pages read (a cap also stops
// the loop when a stack has thousands of events).
export async function cfnPages<T>(
	s: CfnScope,
	operation: string,
	key: string,
	input: Record<string, unknown> = {},
	limit = Number.POSITIVE_INFINITY,
): Promise<T[]> {
	const items: T[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	for (let page = 0; page < limit; page++) {
		const out = await cfnCall<Record<string, unknown>>(s, operation, {
			...input,
			...(token ? { NextToken: token } : {}),
		});
		items.push(...((out[key] as T[] | undefined) ?? []));
		token = out.NextToken as string | undefined;
		if (!token) break;
		if (seen.has(token))
			throw new Error("The server returned a repeated page token.");
		seen.add(token);
	}
	return items;
}

// describeChangeSet reads every page of a change set's Changes.
export async function describeChangeSet(
	s: CfnScope,
	name: string,
): Promise<ChangeSetDetail> {
	const input = { StackName: s.stack, ChangeSetName: name };
	const first = await cfnCall<ChangeSetDetail & { NextToken?: string }>(
		s,
		"DescribeChangeSet",
		input,
	);
	const rest = first.NextToken
		? await cfnPages<NonNullable<ChangeSetDetail["Changes"]>[number]>(
				s,
				"DescribeChangeSet",
				"Changes",
				input,
			)
		: undefined;
	return { ...first, Changes: rest ?? first.Changes };
}

// Validate the exact template before exposing its declarations to the editor.
export async function inspectTemplate(
	s: CfnScope,
	body: string,
	previous: boolean,
) {
	const template = previous
		? String(
				(await cfnCall(s, "GetTemplate", { StackName: s.stack }))
					.TemplateBody ?? "",
			)
		: body;
	await cfnCall(s, "ValidateTemplate", { TemplateBody: template });
	return cfnCall<TemplateSummary>(s, "GetTemplateSummary", {
		TemplateBody: template,
	});
}
export function saveStack(
	s: CfnScope,
	operation: "CreateStack" | "UpdateStack" | "CreateChangeSet",
	input: Record<string, unknown>,
) {
	return cfnCall<{ StackId?: string }>(s, operation, input);
}
