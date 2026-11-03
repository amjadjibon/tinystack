import { tagList } from "./tags.ts";
export const nodeTypes = [
	"cache.t4g.micro",
	"cache.t4g.small",
	"cache.t4g.medium",
	"cache.t3.micro",
	"cache.t3.small",
	"cache.t3.medium",
	"cache.m7g.large",
	"cache.m6g.large",
	"cache.r7g.large",
	"cache.r6g.large",
];

export type EngineVersion = {
	Engine: string;
	EngineVersion: string;
	CacheParameterGroupFamily?: string;
};

// validateCacheName checks the identifier rules shared by clusters,
// replication groups and serverless caches.
export function validateCacheName(name: string, label: string, max: number) {
	if (name.length < 1 || name.length > max)
		throw new Error(`${label} must be 1 to ${max} characters.`);
	if (!/^[a-z][a-z0-9-]*$/.test(name))
		throw new Error(
			`${label} must start with a lowercase letter and contain only lowercase letters, numbers and hyphens.`,
		);
	if (name.includes("--") || name.endsWith("-"))
		throw new Error(
			`${label} cannot contain consecutive hyphens or end with a hyphen.`,
		);
}

export function validateGroupName(name: string, label: string) {
	if (!/^[a-zA-Z][a-zA-Z0-9-]*$/.test(name) || name.length > 255)
		throw new Error(
			`${label} must start with a letter, contain only letters, numbers and hyphens, and have at most 255 characters.`,
		);
	if (name.toLowerCase().startsWith("default"))
		throw new Error(`${label} cannot start with default.`);
}

export function parseCount(
	text: string,
	label: string,
	min: number,
	max: number,
) {
	if (!/^\d+$/.test(text) || Number(text) < min || Number(text) > max)
		throw new Error(`${label} must be an integer from ${min} to ${max}.`);
	return Number(text);
}

export const validatePort = (text: string) =>
	parseCount(text, "Port", 1, 65535);

export const defaultPort = (engine: string) =>
	engine === "memcached" ? 11211 : 6379;

export const engineNames = (versions: EngineVersion[]) =>
	[...new Set(versions.map((v) => v.Engine))].sort();

// compareVersions orders dotted engine versions numerically, so 7.10 sorts after 7.9.
export const compareVersions = (a: string, b: string) =>
	a.localeCompare(b, undefined, { numeric: true });

// versionsFor lists an engine's versions, oldest first.
export const versionsFor = (versions: EngineVersion[], engine: string) =>
	versions
		.filter((v) => v.Engine === engine)
		.map((v) => v.EngineVersion)
		.sort(compareVersions);

// upgradeVersions lists the versions a cache can move to: its own and later ones.
export const upgradeVersions = (
	versions: EngineVersion[],
	engine: string,
	current: string,
) =>
	versionsFor(versions, engine).filter((v) => compareVersions(v, current) >= 0);

export const familiesFor = (versions: EngineVersion[]) =>
	[
		...new Set(
			versions.map((v) => v.CacheParameterGroupFamily ?? "").filter(Boolean),
		),
	].sort();

const settled = new Set(["available", "create-failed", "incompatible-network"]);

// isPendingStatus reports whether a status is still moving, so the page polls.
export function isPendingStatus(status?: string) {
	return !!status && !settled.has(status.toLowerCase());
}

export function hasPending(statuses: (string | undefined)[]) {
	return statuses.some(isPendingStatus);
}

export function statusType(status?: string) {
	const s = (status ?? "").toLowerCase();
	if (s === "available" || s === "active") return "success" as const;
	if (s.includes("fail") || s.includes("incompatible")) return "error" as const;
	return isPendingStatus(s) ? ("loading" as const) : ("pending" as const);
}

export function endpointText(
	endpoint?: { Address?: string; Port?: number } | string,
) {
	if (!endpoint) return "";
	if (typeof endpoint === "string") return endpoint;
	if (!endpoint.Address || !endpoint.Port) return "";
	return `${endpoint.Address}:${endpoint.Port}`;
}

export function cliCommand(engine: string, endpoint: string) {
	const [host, port] = endpoint.split(":");
	if (!host || !port) return "";
	if (engine === "memcached")
		return `printf 'stats\\r\\nquit\\r\\n' | nc ${host} ${port}`;
	return `${engine === "valkey" ? "valkey-cli" : "redis-cli"} -h ${host} -p ${port}`;
}

export type ClusterForm = {
	id: string;
	engine: string;
	version: string;
	nodeType: string;
	nodes: string;
	port: string;
	subnetGroup: string;
	parameterGroup: string;
	securityGroups: string[];
	tags: Record<string, string>;
};

export function clusterCreateInput(f: ClusterForm) {
	validateCacheName(f.id, "Cluster ID", 50);
	if (!f.engine) throw new Error("Choose an engine.");
	if (!f.version) throw new Error("Choose an engine version.");
	if (!f.nodeType) throw new Error("Choose a node type.");
	const nodes =
		f.engine === "memcached" ? parseCount(f.nodes, "Nodes", 1, 40) : 1;
	return {
		CacheClusterId: f.id,
		Engine: f.engine,
		EngineVersion: f.version,
		CacheNodeType: f.nodeType,
		NumCacheNodes: nodes,
		Port: validatePort(f.port),
		...(f.subnetGroup ? { CacheSubnetGroupName: f.subnetGroup } : {}),
		...(f.parameterGroup ? { CacheParameterGroupName: f.parameterGroup } : {}),
		...(f.securityGroups.length ? { SecurityGroupIds: f.securityGroups } : {}),
		Tags: tagList(f.tags),
	};
}

// clusterModifyInput sends only what changed; the service upgrades engine
// versions only and resizes Memcached clusters only.
export function clusterModifyInput(
	current: { Engine: string; EngineVersion: string; NumCacheNodes: number },
	next: { version: string; nodes: string; parameterGroup: string },
) {
	const input: Record<string, unknown> = { ApplyImmediately: true };
	if (next.version && next.version !== current.EngineVersion)
		input.EngineVersion = next.version;
	if (current.Engine === "memcached") {
		const nodes = parseCount(next.nodes, "Nodes", 1, 40);
		if (nodes !== current.NumCacheNodes) input.NumCacheNodes = nodes;
	}
	if (next.parameterGroup) input.CacheParameterGroupName = next.parameterGroup;
	if (Object.keys(input).length === 1)
		throw new Error("Change at least one setting.");
	return input;
}

export type GroupForm = {
	id: string;
	description: string;
	engine: string;
	version: string;
	nodeType: string;
	replicas: string;
	failover: boolean;
	port: string;
	subnetGroup: string;
	parameterGroup: string;
	securityGroups: string[];
	tags: Record<string, string>;
};

export function groupCreateInput(f: GroupForm) {
	validateCacheName(f.id, "Replication group ID", 40);
	if (!f.description.trim()) throw new Error("Description is required.");
	if (!f.engine || !f.version || !f.nodeType)
		throw new Error("Choose an engine, version and node type.");
	const replicas = parseCount(f.replicas, "Replicas", 0, 5);
	if (f.failover && replicas < 1)
		throw new Error("Automatic failover requires at least one replica.");
	return {
		ReplicationGroupId: f.id,
		ReplicationGroupDescription: f.description.trim(),
		Engine: f.engine,
		EngineVersion: f.version,
		CacheNodeType: f.nodeType,
		NumCacheClusters: replicas + 1,
		AutomaticFailoverEnabled: f.failover,
		Port: validatePort(f.port),
		...(f.subnetGroup ? { CacheSubnetGroupName: f.subnetGroup } : {}),
		...(f.parameterGroup ? { CacheParameterGroupName: f.parameterGroup } : {}),
		...(f.securityGroups.length ? { SecurityGroupIds: f.securityGroups } : {}),
		Tags: tagList(f.tags),
	};
}

// replicaCountStep maps a requested replica count to the single scaling call.
export function replicaCountStep(current: number, text: string) {
	const wanted = parseCount(text, "Replicas", 0, 5);
	if (wanted === current) throw new Error("Replica count is unchanged.");
	return {
		operation:
			wanted > current ? "IncreaseReplicaCount" : "DecreaseReplicaCount",
		input: { NewReplicaCount: wanted, ApplyImmediately: true },
	};
}

export function serverlessCreateInput(f: {
	name: string;
	engine: string;
	description: string;
	subnets: string[];
	securityGroups: string[];
	tags: Record<string, string>;
}) {
	validateCacheName(f.name, "Serverless cache name", 40);
	if (!f.engine) throw new Error("Choose an engine.");
	return {
		ServerlessCacheName: f.name,
		Engine: f.engine,
		...(f.description.trim() ? { Description: f.description.trim() } : {}),
		...(f.subnets.length ? { SubnetIds: f.subnets } : {}),
		...(f.securityGroups.length ? { SecurityGroupIds: f.securityGroups } : {}),
		Tags: tagList(f.tags),
	};
}

export const isDefaultGroup = (name?: string) =>
	(name ?? "").startsWith("default.");
