// Pure helpers for the MemoryDB page. They mirror the limits that
// internal/services/memorydb enforces so the form rejects bad input before a
// request is sent.

export const openAccessACL = "open-access";
export const defaultUserName = "default";
export const maxShards = 500;
export const maxReplicas = 5;
export const defaultPort = 6379;

export const nodeTypes = [
	"db.t4g.small",
	"db.t4g.medium",
	"db.r6g.large",
	"db.r6g.xlarge",
	"db.r6g.2xlarge",
	"db.r6g.4xlarge",
	"db.r7g.large",
	"db.r7g.xlarge",
	"db.r7g.2xlarge",
	"db.r7g.4xlarge",
];

export type EngineOption = {
	engine: string;
	version: string;
	parameterGroup: string;
	label: string;
};

// The order is the upgrade order: a cluster can move to a later entry only.
export const engineOptions: EngineOption[] = [
	{
		engine: "redis",
		version: "6.2",
		parameterGroup: "default.memorydb-redis6",
	},
	{
		engine: "redis",
		version: "7.0",
		parameterGroup: "default.memorydb-redis7",
	},
	{
		engine: "redis",
		version: "7.1",
		parameterGroup: "default.memorydb-redis7",
	},
	{
		engine: "valkey",
		version: "7.2",
		parameterGroup: "default.memorydb-valkey7",
	},
].map((e) => ({ ...e, label: `${e.engine} ${e.version}` }));

export function engineKey(engine: string, version: string) {
	return `${engine} ${version}`;
}

export function findEngine(key: string) {
	return engineOptions.find((e) => e.label === key);
}

// The engine versions a cluster at current can move to, including itself.
export function upgradeTargets(engine: string, version: string) {
	const at = engineOptions.findIndex(
		(e) => e.engine === engine && e.version === version,
	);
	return at < 0 ? engineOptions : engineOptions.slice(at);
}

export const subnetGroupNone = "(none)";

export type ClusterForm = {
	name: string;
	description: string;
	nodeType: string;
	engine: string;
	shards: string;
	replicas: string;
	acl: string;
	subnetGroup: string;
	tls: boolean;
	port: string;
};

export function validName(name: string, what: string) {
	if (
		!/^[A-Za-z][A-Za-z0-9-]{0,39}$/.test(name) ||
		name.includes("--") ||
		name.endsWith("-")
	)
		throw new Error(
			`${what} must be 1 to 40 alphanumeric characters or hyphens, begin with a letter, not contain two consecutive hyphens and not end with a hyphen.`,
		);
	return name;
}

export function wholeNumber(
	text: string,
	label: string,
	min: number,
	max: number,
) {
	if (!/^\d+$/.test(text.trim()) || Number(text) < min || Number(text) > max)
		throw new Error(`${label} must be a whole number from ${min} to ${max}.`);
	return Number(text);
}

function checkClusterSettings(f: ClusterForm) {
	if (!f.nodeType) throw new Error("Choose a node type.");
	if (!f.engine || !findEngine(f.engine))
		throw new Error("Choose an engine version.");
	if (!f.acl) throw new Error("Choose an access control list.");
	if (!f.tls && f.acl !== openAccessACL)
		throw new Error(`A cluster without TLS must use the ${openAccessACL} ACL.`);
	return {
		shards: wholeNumber(f.shards, "Number of shards", 1, maxShards),
		replicas: wholeNumber(f.replicas, "Replicas per shard", 0, maxReplicas),
	};
}

export function createClusterInput(f: ClusterForm): Record<string, unknown> {
	const { shards, replicas } = checkClusterSettings(f);
	const engine = findEngine(f.engine);
	return {
		ClusterName: validName(f.name.trim(), "Cluster name"),
		NodeType: f.nodeType,
		Engine: engine?.engine,
		EngineVersion: engine?.version,
		ParameterGroupName: engine?.parameterGroup,
		ACLName: f.acl,
		NumShards: shards,
		NumReplicasPerShard: replicas,
		TLSEnabled: f.tls,
		Port: wholeNumber(f.port, "Port", 1, 65535),
		...(f.description.trim() ? { Description: f.description.trim() } : {}),
		...(f.subnetGroup && f.subnetGroup !== subnetGroupNone
			? { SubnetGroupName: f.subnetGroup }
			: {}),
	};
}

export type ClusterLike = {
	Name?: string;
	Description?: string;
	NodeType?: string;
	Engine?: string;
	EngineVersion?: string;
	NumberOfShards?: number;
	AvailabilityMode?: string;
	ACLName?: string;
	ParameterGroupName?: string;
	TLSEnabled?: boolean;
	Status?: string;
	SubnetGroupName?: string;
	ClusterEndpoint?: { Address?: string; Port?: number };
	ARN?: string;
	Shards?: { NumberOfNodes?: number }[];
};

// updateClusterInput sends only what changed. The replica count is not part of
// DescribeClusters, so it is sent when the form value differs from the one the
// page last knew (current).
export function updateClusterInput(
	cluster: ClusterLike,
	f: ClusterForm,
	currentReplicas: number,
): Record<string, unknown> {
	const { shards, replicas } = checkClusterSettings(f);
	const engine = findEngine(f.engine);
	const input: Record<string, unknown> = { ClusterName: cluster.Name };
	if (f.nodeType !== cluster.NodeType) input.NodeType = f.nodeType;
	if (
		engine &&
		(engine.engine !== cluster.Engine ||
			engine.version !== cluster.EngineVersion)
	) {
		input.Engine = engine.engine;
		input.EngineVersion = engine.version;
	}
	if (engine && engine.parameterGroup !== cluster.ParameterGroupName)
		input.ParameterGroupName = engine.parameterGroup;
	if (f.acl !== cluster.ACLName) input.ACLName = f.acl;
	if (f.description.trim() !== (cluster.Description ?? "").trim())
		input.Description = f.description.trim();
	if (shards !== cluster.NumberOfShards)
		input.ShardConfiguration = { ShardCount: shards };
	if (replicas !== currentReplicas)
		input.ReplicaConfiguration = { ReplicaCount: replicas };
	if (Object.keys(input).length === 1)
		throw new Error("Change at least one setting.");
	return input;
}

// replicasPerShard reads the replica count from the first shard of a detailed
// cluster description: one node is the primary, the rest are replicas.
export function replicasPerShard(cluster: ClusterLike) {
	const nodes = cluster.Shards?.[0]?.NumberOfNodes;
	return typeof nodes === "number" && nodes > 0 ? nodes - 1 : 0;
}

const pendingStatuses = new Set([
	"creating",
	"updating",
	"deleting",
	"snapshotting",
]);

export function isPendingStatus(status?: string) {
	return pendingStatuses.has((status ?? "").toLowerCase());
}

export function hasPendingClusters(clusters: { Status?: string }[]) {
	return clusters.some((c) => isPendingStatus(c.Status));
}

export function endpointText(c: ClusterLike) {
	const e = c.ClusterEndpoint;
	return e?.Address ? `${e.Address}:${e.Port ?? defaultPort}` : "";
}

export function describeShape(c: ClusterLike) {
	const shards = c.NumberOfShards ?? 0;
	const replicas = replicasPerShard(c);
	return `${shards} shard${shards === 1 ? "" : "s"}, ${replicas} replica${replicas === 1 ? "" : "s"} per shard`;
}

// Passwords are masked in tables and forms; only the count is ever returned.
export function maskSecret(secret: string) {
	return "•".repeat(Math.min(secret.length, 12));
}

export function authSummary(auth?: { Type?: string; PasswordCount?: number }) {
	if (!auth?.Type) return "";
	if (auth.Type === "password")
		return `password (${auth.PasswordCount ?? 0} set, hidden)`;
	return auth.Type;
}

export function validatePasswords(passwords: string[]) {
	const list = passwords.filter((p) => p !== "");
	if (list.length < 1 || list.length > 2)
		throw new Error("A password user needs one or two passwords.");
	if (new Set(list).size !== list.length)
		throw new Error("The two passwords must differ.");
	for (const p of list)
		if (
			p.length < 16 ||
			p.length > 128 ||
			/[,"/@]/.test(p) ||
			/[^\x20-\x7e]/.test(p)
		)
			throw new Error(
				'Passwords must be 16 to 128 printable characters without , " / or @.',
			);
	return list;
}

export function validateAccessString(access: string) {
	const text = access.trim();
	if (!text) throw new Error("Enter an access string, such as on ~* +@all.");
	if (text.split(/\s+/).some((rule) => rule.toLowerCase() === "reset"))
		throw new Error("The access string cannot contain the reset command.");
	return text;
}

export type UserForm = {
	name: string;
	access: string;
	auth: "password" | "iam";
	passwords: string[];
};

export function createUserInput(f: UserForm): Record<string, unknown> {
	return {
		UserName: validName(f.name.trim(), "User name"),
		AccessString: validateAccessString(f.access),
		AuthenticationMode:
			f.auth === "iam"
				? { Type: "iam" }
				: { Type: "password", Passwords: validatePasswords(f.passwords) },
	};
}

// updateUserInput leaves passwords alone when none are typed.
export function updateUserInput(
	name: string,
	f: Omit<UserForm, "name">,
	currentAccess: string,
	changeAuth: boolean,
): Record<string, unknown> {
	const input: Record<string, unknown> = { UserName: name };
	const access = validateAccessString(f.access);
	if (access !== currentAccess) input.AccessString = access;
	if (changeAuth)
		input.AuthenticationMode =
			f.auth === "iam"
				? { Type: "iam" }
				: { Type: "password", Passwords: validatePasswords(f.passwords) };
	if (Object.keys(input).length === 1)
		throw new Error("Change the access string or the authentication.");
	return input;
}

export function aclMembershipChange(current: string[], next: string[]) {
	return {
		add: next.filter((u) => !current.includes(u)),
		remove: current.filter((u) => !next.includes(u)),
	};
}

export function updateACLInput(
	name: string,
	current: string[],
	next: string[],
): Record<string, unknown> {
	const { add, remove } = aclMembershipChange(current, next);
	if (!add.length && !remove.length)
		throw new Error("Add or remove at least one user.");
	return {
		ACLName: name,
		...(add.length ? { UserNamesToAdd: add } : {}),
		...(remove.length ? { UserNamesToRemove: remove } : {}),
	};
}

export type SubnetLike = {
	SubnetId?: string;
	VpcId?: string;
	AvailabilityZone?: string;
	CidrBlock?: string;
};

export function subnetLabel(s: SubnetLike) {
	return `${s.SubnetId} (${s.AvailabilityZone ?? "?"}, ${s.CidrBlock ?? "?"})`;
}

// Every subnet of a group must share one VPC.
export function sameVpc(subnets: SubnetLike[], ids: string[]) {
	const vpcs = new Set(
		ids.map((id) => subnets.find((s) => s.SubnetId === id)?.VpcId),
	);
	return vpcs.size <= 1;
}

export function subnetGroupInput(
	name: string | undefined,
	description: string,
	ids: string[],
	subnets: SubnetLike[],
): Record<string, unknown> {
	if (!ids.length) throw new Error("Choose at least one subnet.");
	if (ids.length > 20)
		throw new Error("A subnet group has at most 20 subnets.");
	if (!sameVpc(subnets, ids))
		throw new Error("All subnets must belong to the same VPC.");
	return {
		...(name !== undefined
			? { SubnetGroupName: validName(name.trim(), "Subnet group name") }
			: {}),
		...(description.trim() ? { Description: description.trim() } : {}),
		SubnetIds: ids,
	};
}

export function tagList(tags: Record<string, string>) {
	return Object.entries(tags).map(([Key, Value]) => ({ Key, Value }));
}

export function tagMap(list: { Key?: string; Value?: string }[] | undefined) {
	return Object.fromEntries(
		(list ?? []).map((t) => [t.Key ?? "", t.Value ?? ""]),
	);
}
