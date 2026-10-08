import { api, type Resource } from "./api";

export type IAMKind = "user" | "group" | "role" | "policy";
export type IAMSection = IAMKind | "overview";
export const iamKinds: IAMKind[] = ["user", "group", "role", "policy"];
export const iamLabels: Record<IAMKind, { singular: string; plural: string }> =
	{
		user: { singular: "User", plural: "Users" },
		group: { singular: "User group", plural: "User groups" },
		role: { singular: "Role", plural: "Roles" },
		policy: { singular: "Policy", plural: "Policies" },
	};
export const iamEntity: Record<IAMKind, string> = {
	user: "User",
	group: "Group",
	role: "Role",
	policy: "Policy",
};
export function iamHref(
	section: IAMSection = "overview",
	account = "",
	name = "",
) {
	return `/services/iam?${new URLSearchParams({ section, account, name })}`;
}
export function iamInput(resource: Resource): Record<string, unknown> {
	return resource.type === "policy"
		? { PolicyArn: resource.arn }
		: { [`${iamEntity[resource.type as IAMKind]}Name`]: resource.name };
}
export function iamCall<T = Record<string, unknown>>(
	account: string,
	operation: string,
	input: Record<string, unknown>,
) {
	return api.action<T>("iam", operation, account, input);
}
export async function iamPages<T>(
	account: string,
	operation: string,
	input: Record<string, unknown>,
	key: string,
): Promise<T[]> {
	const items: T[] = [];
	let marker: string | undefined;
	do {
		const result = await iamCall<{
			IsTruncated?: boolean;
			Marker?: string;
			[key: string]: unknown;
		}>(account, operation, {
			...input,
			MaxItems: 1000,
			...(marker && { Marker: marker }),
		});
		items.push(...((result[key] as T[] | undefined) ?? []));
		marker = result.IsTruncated ? result.Marker : undefined;
	} while (marker);
	return items;
}
export function policyText(text: unknown): string {
	if (typeof text !== "string") return "";
	try {
		return JSON.stringify(JSON.parse(text), null, 2);
	} catch {
		try {
			return JSON.stringify(
				JSON.parse(decodeURIComponent(text.replace(/\+/g, " "))),
				null,
				2,
			);
		} catch {
			return text;
		}
	}
}
export const emptyPolicy = JSON.stringify(
	{ Version: "2012-10-17", Statement: [] },
	null,
	2,
);
export const defaultTrust = JSON.stringify(
	{
		Version: "2012-10-17",
		Statement: [
			{
				Effect: "Allow",
				Principal: { Service: "lambda.amazonaws.com" },
				Action: "sts:AssumeRole",
			},
		],
	},
	null,
	2,
);

export type IAMStep = {
	id: string;
	label: string;
	run: () => Promise<unknown>;
};
// Keep completed steps for the lifetime of a form so retries do not repeat successful writes.
export async function iamSequence(steps: IAMStep[], completed: Set<string>) {
	for (const step of steps) {
		if (completed.has(step.id)) continue;
		try {
			await step.run();
			completed.add(step.id);
		} catch (err) {
			throw new Error(
				`${completed.size} of ${steps.length} steps completed. ${step.label}: ${err instanceof Error ? err.message : String(err)} Retry to continue the remaining steps, or close and manage the saved changes from the resource page.`,
			);
		}
	}
}
export function iamGroupsForUser(resources: Resource[], name: string) {
	return resources.filter(
		(r) =>
			r.type === "group" &&
			((r.details?.Users as string[] | undefined) ?? []).some(
				(u) => u.toLowerCase() === name.toLowerCase(),
			),
	);
}
