import { api } from "./api";

export type ElastiCacheScope = {
	account: string;
	region: string;
	section?: string;
};
export type Tag = { Key: string; Value: string };
export type Endpoint = { Address?: string; Port?: number };
export type CacheNode = {
	CacheNodeId: string;
	CacheNodeStatus?: string;
	Endpoint?: Endpoint;
};
export type CacheCluster = {
	CacheClusterId: string;
	ARN: string;
	Engine: string;
	EngineVersion: string;
	CacheNodeType: string;
	CacheClusterStatus: string;
	NumCacheNodes: number;
	CacheClusterCreateTime?: string | number;
	ConfigurationEndpoint?: Endpoint;
	CacheNodes?: CacheNode[];
	CacheSubnetGroupName?: string;
	CacheParameterGroup?: { CacheParameterGroupName: string };
	ReplicationGroupId?: string;
};
export type NodeGroupMember = {
	CacheClusterId: string;
	CurrentRole?: string;
	ReadEndpoint?: Endpoint;
};
export type ReplicationGroup = {
	ReplicationGroupId: string;
	ARN: string;
	Description?: string;
	Status: string;
	Engine?: string;
	CacheNodeType?: string;
	MemberClusters?: string[];
	AutomaticFailover?: string;
	MultiAZ?: string;
	NodeGroups?: {
		NodeGroupId: string;
		PrimaryEndpoint?: Endpoint;
		ReaderEndpoint?: Endpoint;
		NodeGroupMembers?: NodeGroupMember[];
	}[];
	ReplicationGroupCreateTime?: string | number;
};
export type ServerlessCache = {
	ServerlessCacheName: string;
	ARN: string;
	Description?: string;
	Status: string;
	Engine: string;
	MajorEngineVersion?: string;
	FullEngineVersion?: string;
	Endpoint?: Endpoint;
	ReaderEndpoint?: Endpoint;
	SubnetIds?: string[];
	SecurityGroupIds?: string[];
	CreateTime?: string | number;
};
export type SubnetGroup = {
	CacheSubnetGroupName: string;
	CacheSubnetGroupDescription?: string;
	VpcId?: string;
	ARN?: string;
	Subnets?: { SubnetIdentifier: string }[];
};
export type ParameterGroup = {
	CacheParameterGroupName: string;
	CacheParameterGroupFamily?: string;
	Description?: string;
	ARN?: string;
};
export type CacheParameter = {
	ParameterName: string;
	ParameterValue?: string;
	Description?: string;
	Source?: string;
	IsModifiable?: boolean;
	AllowedValues?: string;
};
export type CacheEvent = {
	SourceIdentifier?: string;
	SourceType?: string;
	Message?: string;
	Date?: string | number;
};

export function elasticacheHref(s: ElastiCacheScope & { section?: string }) {
	const p = new URLSearchParams({ account: s.account, region: s.region });
	if (s.section) p.set("section", s.section);
	return `/services/elasticache?${p}`;
}

export function ecCall<T = Record<string, unknown>>(
	s: ElastiCacheScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("elasticache", operation, s.account, input, s.region);
}

// ecPages follows Marker pagination; a repeated marker is an error rather than a loop.
export async function ecPages<T>(
	s: ElastiCacheScope,
	operation: string,
	key: string,
	input: Record<string, unknown> = {},
): Promise<T[]> {
	const items: T[] = [],
		seen = new Set<string>();
	let marker: string | undefined;
	do {
		const out = await ecCall<Record<string, unknown>>(s, operation, {
			...input,
			MaxRecords: 100,
			...(marker ? { Marker: marker } : {}),
		});
		items.push(...((out[key] as T[] | undefined) ?? []));
		marker = out.Marker as string | undefined;
		if (marker && seen.has(marker))
			throw new Error("The server returned a repeated page marker.");
		if (marker) seen.add(marker);
	} while (marker);
	return items;
}

export async function serverlessPages(
	s: ElastiCacheScope,
): Promise<ServerlessCache[]> {
	const items: ServerlessCache[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const out = await ecCall<{
			ServerlessCaches?: ServerlessCache[];
			NextToken?: string;
		}>(s, "DescribeServerlessCaches", {
			MaxResults: 50,
			...(token ? { NextToken: token } : {}),
		});
		items.push(...(out.ServerlessCaches ?? []));
		token = out.NextToken;
		if (token && seen.has(token))
			throw new Error("The server returned a repeated page token.");
		if (token) seen.add(token);
	} while (token);
	return items;
}

export function ecDate(value?: string | number) {
	if (value === undefined) return "—";
	const date = new Date(typeof value === "number" ? value * 1000 : value);
	return Number.isNaN(date.getTime()) ? "—" : date.toISOString();
}

export const runtimeNote = (runtime?: string) =>
	runtime === "docker"
		? "Cache nodes run as containers; endpoints show the allocated host ports and status follows readiness probes."
		: "Clusters, replication groups and serverless caches are stored as records; no cache server listens on the endpoint. Set elasticache.caches: docker to run real nodes.";

export {
	cliCommand,
	defaultPort,
	type EngineVersion,
	endpointText,
	engineNames,
	familiesFor,
	isDefaultGroup,
	isPendingStatus,
	nodeTypes,
	statusType,
	upgradeVersions,
	versionsFor,
} from "./elasticacheHelpers";
