import { api } from "./api";
import type { Cluster, EKSScope } from "./eksHelpers";
export const eksCall = <T = Record<string, unknown>>(
	s: EKSScope,
	op: string,
	input: Record<string, unknown> = {},
): Promise<T> => api.action("eks", op, s.account, input, s.region);
export async function eksList<T>(
	s: EKSScope,
	op: string,
	key: string,
	input: Record<string, unknown> = {},
) {
	const items: T[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const out = await eksCall<Record<string, unknown>>(s, op, {
			...input,
			MaxResults: 100,
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
export async function clusters(s: EKSScope) {
	const names = await eksList<string>(s, "ListClusters", "Clusters");
	return Promise.all(
		names.map(
			async (Name) =>
				(await eksCall<{ Cluster: Cluster }>(s, "DescribeCluster", { Name }))
					.Cluster,
		),
	);
}
export function eksHref(s: EKSScope, cluster = "", tab = "overview") {
	const params = new URLSearchParams({ ...s, tab });
	if (cluster) params.set("cluster", cluster);
	return `/services/eks?${params}`;
}
