import { api } from "./api";
import {
	callLimit,
	type Identity,
	type IdentityPool,
} from "./cognitoidentityHelpers";

export type IdentityScope = { account: string; region: string };

export function identityHref(s: IdentityScope, pool = "") {
	const query = new URLSearchParams({ account: s.account, region: s.region });
	if (pool) query.set("pool", pool);
	return `/services/cognitoidentity?${query}`;
}

export function identityCall<T = Record<string, unknown>>(
	s: IdentityScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("cognitoidentity", operation, s.account, input, s.region);
}

async function identityPages<T>(
	s: IdentityScope,
	operation: "ListIdentityPools" | "ListIdentities",
	key: string,
	input: Record<string, unknown> = {},
): Promise<T[]> {
	const items: T[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const out = await identityCall<Record<string, unknown>>(s, operation, {
			...input,
			MaxResults: callLimit,
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

// listIdentityPools describes each pool, because the list gives only names and
// IDs, and orders them by name, because the service orders them by random ID.
export async function listIdentityPools(
	s: IdentityScope,
): Promise<IdentityPool[]> {
	const pools = await identityPages<{ IdentityPoolId: string }>(
		s,
		"ListIdentityPools",
		"IdentityPools",
	);
	const described = await Promise.all(
		pools.map((p) =>
			identityCall<IdentityPool>(s, "DescribeIdentityPool", {
				IdentityPoolId: p.IdentityPoolId,
			}),
		),
	);
	return described.sort((a, b) =>
		a.IdentityPoolName.localeCompare(b.IdentityPoolName),
	);
}

export function listIdentities(
	s: IdentityScope,
	pool: string,
): Promise<Identity[]> {
	return identityPages<Identity>(s, "ListIdentities", "Identities", {
		IdentityPoolId: pool,
	});
}
