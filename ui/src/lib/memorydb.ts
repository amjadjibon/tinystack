import { api } from "./api";
import type { ClusterLike, SubnetLike } from "./memorydbHelpers";

export type MemoryDBScope = { account: string; region: string };
export type Cluster = ClusterLike & { Name: string; ARN: string };
export type User = {
	Name: string;
	Status?: string;
	AccessString?: string;
	ACLNames?: string[];
	Authentication?: { Type?: string; PasswordCount?: number };
	ARN: string;
};
export type ACL = {
	Name: string;
	Status?: string;
	UserNames?: string[];
	Clusters?: string[];
	ARN: string;
};
export type SubnetGroup = {
	Name: string;
	Description?: string;
	VpcId?: string;
	Subnets?: { Identifier?: string; AvailabilityZone?: { Name?: string } }[];
	ARN: string;
};
export function memorydbHref(s: MemoryDBScope) {
	return `/services/memorydb?${new URLSearchParams({ account: s.account, region: s.region })}`;
}
export function memorydbCall<T = Record<string, unknown>>(
	s: MemoryDBScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("memorydb", operation, s.account, input, s.region);
}
export async function memorydbPages<T>(
	s: MemoryDBScope,
	operation: string,
	key: string,
	input: Record<string, unknown> = {},
): Promise<T[]> {
	const items: T[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const out = await memorydbCall<Record<string, unknown>>(s, operation, {
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
export const listClusters = (s: MemoryDBScope) =>
	memorydbPages<Cluster>(s, "DescribeClusters", "Clusters", {
		ShowShardDetails: true,
	});
export const listUsers = (s: MemoryDBScope) =>
	memorydbPages<User>(s, "DescribeUsers", "Users");
export const listACLs = (s: MemoryDBScope) =>
	memorydbPages<ACL>(s, "DescribeACLs", "ACLs");
export const listSubnetGroups = (s: MemoryDBScope) =>
	memorydbPages<SubnetGroup>(s, "DescribeSubnetGroups", "SubnetGroups");
// Subnets come from EC2, which the subnet group references.
export async function listSubnets(s: MemoryDBScope): Promise<SubnetLike[]> {
	const out = await api.action<{ Subnets?: SubnetLike[] }>(
		"ec2",
		"DescribeSubnets",
		s.account,
		{},
		s.region,
	);
	return out.Subnets ?? [];
}
