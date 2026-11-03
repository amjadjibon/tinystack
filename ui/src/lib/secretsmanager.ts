import { api } from "./api";
import type { ActionStep } from "./consoleActions";
import { tagList } from "./tags.ts";

export type SecretScope = { account: string; region: string; secret?: string };
export type Secret = {
	ARN: string;
	Name: string;
	Description?: string;
	KmsKeyId?: string;
	CreatedDate?: number;
	LastChangedDate?: number;
	DeletedDate?: number;
	Tags?: { Key: string; Value: string }[];
	VersionIdsToStages?: Record<string, string[]>;
	RotationEnabled?: boolean;
	RotationLambdaARN?: string;
	RotationRules?: {
		AutomaticallyAfterDays?: number;
		ScheduleExpression?: string;
		Duration?: string;
	};
	LastRotatedDate?: number;
	NextRotationDate?: number;
};
export type SecretVersion = {
	VersionId: string;
	VersionStages?: string[];
	CreatedDate?: number;
};
export type SecretValue = SecretVersion & {
	SecretString?: string;
	SecretBinary?: string;
};
export function secretHref(s: SecretScope) {
	const params = new URLSearchParams({ account: s.account, region: s.region });
	if (s.secret) params.set("secret", s.secret);
	return `/services/secretsmanager?${params}`;
}
export function secretCall<T = Record<string, unknown>>(
	s: SecretScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action(
		"secretsmanager",
		operation,
		s.account,
		{ ...(s.secret ? { SecretId: s.secret } : {}), ...input },
		s.region,
	);
}
// Metadata pages are collected before filtering and sorting; values never enter this cache.
export async function secretPages<T>(
	s: SecretScope,
	operation: string,
	key: string,
	input: Record<string, unknown> = {},
): Promise<T[]> {
	const items: T[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const out = await secretCall<Record<string, unknown>>(s, operation, {
			...input,
			MaxResults: 100,
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
export const secretDate = (v?: number) =>
	v === undefined ? "—" : new Date(v * 1000).toISOString();
export function secretTagPlan(
	s: SecretScope,
	tags: Record<string, string>,
	removed: string[],
): ActionStep[] {
	return [
		...(removed.length
			? [
					{
						label: "Remove tags",
						run: () => secretCall(s, "UntagResource", { TagKeys: removed }),
					},
				]
			: []),
		...(Object.keys(tags).length
			? [
					{
						label: "Save tags",
						run: () => secretCall(s, "TagResource", { Tags: tagList(tags) }),
					},
				]
			: []),
	];
}
export function valueInput(
	format: string,
	text: string,
	rows: { key: string; value: string }[],
): Record<string, unknown> {
	if (format === "binary") {
		let decoded: string;
		try {
			decoded = atob(text);
		} catch {
			throw new Error("Binary value must be valid base64.");
		}
		if (decoded.length < 1 || decoded.length > 65536)
			throw new Error("Binary value must contain 1–65,536 decoded bytes.");
		return { SecretBinary: btoa(decoded) };
	}
	if (format === "pairs") {
		if (!rows.length || rows.some((r) => !r.key))
			throw new Error("Enter a key for every key/value pair.");
		if (new Set(rows.map((r) => r.key)).size !== rows.length)
			throw new Error("Secret keys must be unique.");
		text = JSON.stringify(
			Object.fromEntries(rows.map((r) => [r.key, r.value])),
		);
	}
	const length = new TextEncoder().encode(text).length;
	if (length < 1 || length > 65536)
		throw new Error("Secret value must contain 1–65,536 UTF-8 bytes.");
	return { SecretString: text };
}
