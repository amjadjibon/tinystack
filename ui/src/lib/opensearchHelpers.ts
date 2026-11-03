// Pure logic for the OpenSearch Service page: payload builders, checks and display summaries. The limits mirror
// internal/services/opensearch, so a form rejects what the service would refuse before a request is sent.
import { integer, jsonObject } from "./consoleActions.ts";
import { tagsFromRows } from "./elbv2Helpers.ts";
import { tagList } from "./tags.ts";

export type TagRow = { key: string; value: string };

export type ClusterConfig = {
	InstanceType?: string;
	InstanceCount?: number;
	DedicatedMasterEnabled?: boolean;
	DedicatedMasterType?: string;
	DedicatedMasterCount?: number;
	ZoneAwarenessEnabled?: boolean;
};
export type EBSOptions = {
	EBSEnabled?: boolean;
	VolumeType?: string;
	VolumeSize?: number;
	Iops?: number;
	Throughput?: number;
};
export type EncryptionAtRestOptions = { Enabled?: boolean; KmsKeyId?: string };
export type NodeToNodeEncryptionOptions = { Enabled?: boolean };
export type DomainEndpointOptions = {
	EnforceHTTPS?: boolean;
	TLSSecurityPolicy?: string;
};
export type DomainStatus = {
	DomainName: string;
	ARN?: string;
	DomainId?: string;
	EngineVersion?: string;
	Endpoint?: string;
	Created?: boolean;
	Deleted?: boolean;
	Processing?: boolean;
	AccessPolicies?: string;
	AdvancedOptions?: Record<string, string>;
	DomainProcessingStatus?: string;
	ClusterConfig?: ClusterConfig;
	EBSOptions?: EBSOptions;
	EncryptionAtRestOptions?: EncryptionAtRestOptions;
	NodeToNodeEncryptionOptions?: NodeToNodeEncryptionOptions;
	DomainEndpointOptions?: DomainEndpointOptions;
};
// A domain as the list shows it. DescribeDomains carries no creation time, so the page reads it from the
// domain's configuration.
export type DomainRow = DomainStatus & { CreationDate?: number };

export type OptionStatus = {
	CreationDate?: number;
	UpdateDate?: number;
	UpdateVersion?: number;
	State?: string;
	PendingDeletion?: boolean;
};
// The options DescribeDomainConfig reports a status for, with the names the page gives them.
export const configOptions = [
	["EngineVersion", "Engine version"],
	["AdvancedOptions", "Advanced options"],
	["ClusterConfig", "Cluster configuration"],
	["EBSOptions", "EBS storage"],
	["AccessPolicies", "Access policy"],
	["EncryptionAtRestOptions", "Encryption at rest"],
	["NodeToNodeEncryptionOptions", "Node-to-node encryption"],
	["DomainEndpointOptions", "Domain endpoint"],
] as const;
export type ConfigOption = (typeof configOptions)[number][0];
export type DomainConfig = Partial<
	Record<ConfigOption, { Status?: OptionStatus }>
>;

// Choices ------------------------------------------------------------------

// Fallback choices used before version discovery completes.
export const engineVersions = [
	"OpenSearch_3.3",
	"OpenSearch_3.1",
	"OpenSearch_2.19",
	"OpenSearch_2.17",
	"OpenSearch_2.15",
	"OpenSearch_2.13",
	"OpenSearch_2.11",
	"OpenSearch_2.9",
	"OpenSearch_2.7",
	"OpenSearch_2.5",
	"OpenSearch_2.3",
	"OpenSearch_1.3",
	"Elasticsearch_7.10",
];
const enginePattern = /^(OpenSearch|Elasticsearch)_\d+\.\d+$/;

export const instanceTypes = [
	"t3.small.search",
	"t3.medium.search",
	"m5.large.search",
	"m5.xlarge.search",
	"m5.2xlarge.search",
	"m6g.large.search",
	"m6g.xlarge.search",
	"m6g.2xlarge.search",
	"c5.large.search",
	"c5.xlarge.search",
	"r5.large.search",
	"r5.xlarge.search",
	"r6g.large.search",
	"r6g.xlarge.search",
];

export const volumeTypes = ["gp2", "gp3", "io1", "standard"];
export const tlsPolicies = [
	"Policy-Min-TLS-1-0-2019-07",
	"Policy-Min-TLS-1-2-2019-07",
	"Policy-Min-TLS-1-2-PFS-2023-10",
	"Policy-Min-TLS-1-2-RFC9151-FIPS-2024-08",
];

// The service starts a new domain from these settings, so a blank form creates what the SDK would.
export const defaultEngineVersion = "OpenSearch_2.11";
export const defaultInstanceType = "m5.large.search";
export const defaultTLSPolicy = "Policy-Min-TLS-1-0-2019-07";

const domainName = /^[a-z][a-z0-9-]{2,27}$/;
// The bounds below only catch typing mistakes; the service checks no upper limit.
const maxInstances = 1000;
const maxMasters = 9;
const minVolumeGiB = 10;
const maxVolumeGiB = 16384;
const maxIops = 64000;
const maxThroughput = 4000;

// Only provisioned volumes take IOPS, and only gp3 takes throughput.
export const carriesIops = (type: string) => type === "io1" || type === "gp3";
export const carriesThroughput = (type: string) => type === "gp3";

export function advancedOptionsInput(text: string): Record<string, string> {
	const value = jsonObject(text, "Advanced options");
	for (const [key, item] of Object.entries(value)) {
		if (
			![
				"override_main_response_version",
				"rest.action.multi_allow_explicit_index",
				"indices.fielddata.cache.size",
				"indices.query.bool.max_clause_count",
			].includes(key)
		)
			throw new Error(`Unsupported advanced option: ${key}`);
		if (typeof item !== "string")
			throw new Error("Advanced option values must be strings.");
		if (
			[
				"override_main_response_version",
				"rest.action.multi_allow_explicit_index",
			].includes(key) &&
			item !== "true" &&
			item !== "false"
		)
			throw new Error(`${key} must be true or false.`);
		if (key.startsWith("indices.") && !/^\d+$/.test(item))
			throw new Error(`${key} must be a nonnegative integer.`);
	}
	return value as Record<string, string>;
}

export type UpgradeHistory = {
	UpgradeName?: string;
	StartTimestamp?: number;
	UpgradeStatus?: string;
	StepsList?: { UpgradeStep?: string; ProgressPercent?: number }[];
};

export function upgradeTargets(
	compatible: { SourceVersion?: string; TargetVersions?: string[] }[],
	source?: string,
) {
	return (
		compatible.find((entry) => entry.SourceVersion === source)
			?.TargetVersions ?? []
	);
}

// Forms ----------------------------------------------------------------------

export type DomainForm = {
	name: string;
	engineVersion: string;
	instanceType: string;
	instanceCount: string;
	dedicatedMaster: boolean;
	masterType: string;
	masterCount: string;
	zoneAwareness: boolean;
	ebsEnabled: boolean;
	volumeType: string;
	volumeSize: string;
	iops: string;
	throughput: string;
	policy: string;
	advancedOptions: string;
	atRest: boolean;
	kmsKey: string;
	nodeToNode: boolean;
	enforceHTTPS: boolean;
	tlsPolicy: string;
	tags: TagRow[];
};

export function emptyDomainForm(): DomainForm {
	return {
		name: "",
		engineVersion: defaultEngineVersion,
		instanceType: defaultInstanceType,
		instanceCount: "1",
		dedicatedMaster: false,
		masterType: defaultInstanceType,
		masterCount: "3",
		zoneAwareness: false,
		ebsEnabled: true,
		volumeType: "gp2",
		volumeSize: "10",
		iops: "",
		throughput: "",
		policy: "",
		advancedOptions: "{}",
		atRest: false,
		kmsKey: "",
		nodeToNode: false,
		enforceHTTPS: false,
		tlsPolicy: defaultTLSPolicy,
		tags: [],
	};
}

// prettyPolicy lays a stored policy out for editing, and leaves anything that is not JSON as it is.
export function prettyPolicy(policy?: string) {
	if (!policy) return "";
	try {
		return JSON.stringify(JSON.parse(policy), null, 2);
	} catch {
		return policy;
	}
}

// formFromDomain fills the form with the settings a domain has, for the edit dialog.
export function formFromDomain(d: DomainStatus): DomainForm {
	const cluster = d.ClusterConfig;
	const volume = d.EBSOptions;
	return {
		name: d.DomainName,
		engineVersion: d.EngineVersion ?? defaultEngineVersion,
		instanceType: cluster?.InstanceType ?? defaultInstanceType,
		instanceCount: String(cluster?.InstanceCount ?? 1),
		dedicatedMaster: !!cluster?.DedicatedMasterEnabled,
		masterType: cluster?.DedicatedMasterType ?? defaultInstanceType,
		masterCount: String(cluster?.DedicatedMasterCount ?? 3),
		zoneAwareness: !!cluster?.ZoneAwarenessEnabled,
		ebsEnabled: !!volume?.EBSEnabled,
		volumeType: volume?.VolumeType ?? "gp2",
		volumeSize: String(volume?.VolumeSize ?? minVolumeGiB),
		iops: volume?.Iops === undefined ? "" : String(volume.Iops),
		throughput:
			volume?.Throughput === undefined ? "" : String(volume.Throughput),
		policy: prettyPolicy(d.AccessPolicies),
		advancedOptions: JSON.stringify(d.AdvancedOptions ?? {}, null, 2),
		atRest: !!d.EncryptionAtRestOptions?.Enabled,
		kmsKey: d.EncryptionAtRestOptions?.KmsKeyId ?? "",
		nodeToNode: !!d.NodeToNodeEncryptionOptions?.Enabled,
		enforceHTTPS: !!d.DomainEndpointOptions?.EnforceHTTPS,
		tlsPolicy: d.DomainEndpointOptions?.TLSSecurityPolicy ?? defaultTLSPolicy,
		tags: [],
	};
}

// Request builders ---------------------------------------------------------

function clusterConfig(f: DomainForm): ClusterConfig {
	if (!f.instanceType.endsWith(".search"))
		throw new Error("Choose an instance type.");
	const count = integer(
		f.instanceCount.trim(),
		"Instance count",
		1,
		maxInstances,
	);
	if (f.zoneAwareness && count % 2 !== 0)
		throw new Error(
			"Zone awareness spreads the instances over two Availability Zones, so the instance count must be even.",
		);
	const base = {
		InstanceType: f.instanceType,
		InstanceCount: count,
		ZoneAwarenessEnabled: f.zoneAwareness,
	};
	// The service refuses master settings on a domain without dedicated masters, so they are left out.
	if (!f.dedicatedMaster) return { ...base, DedicatedMasterEnabled: false };
	if (!f.masterType.endsWith(".search"))
		throw new Error("Choose a dedicated master instance type.");
	return {
		...base,
		DedicatedMasterEnabled: true,
		DedicatedMasterType: f.masterType,
		DedicatedMasterCount: integer(
			f.masterCount.trim(),
			"Dedicated master count",
			1,
			maxMasters,
		),
	};
}

function ebsOptions(f: DomainForm): EBSOptions {
	if (!f.ebsEnabled) return { EBSEnabled: false };
	if (!volumeTypes.includes(f.volumeType))
		throw new Error("Choose a volume type.");
	const options: EBSOptions = {
		EBSEnabled: true,
		VolumeType: f.volumeType,
		VolumeSize: integer(
			f.volumeSize.trim(),
			"Volume size",
			minVolumeGiB,
			maxVolumeGiB,
		),
	};
	// The fields for IOPS and throughput are hidden for other volume types, so a stale value is left out.
	const iops = f.iops.trim();
	if (iops && carriesIops(f.volumeType))
		options.Iops = integer(iops, "IOPS", 1, maxIops);
	const throughput = f.throughput.trim();
	if (throughput && carriesThroughput(f.volumeType))
		options.Throughput = integer(throughput, "Throughput", 1, maxThroughput);
	return options;
}

function encryptionAtRest(f: DomainForm): EncryptionAtRestOptions {
	if (!f.atRest) return { Enabled: false };
	const key = f.kmsKey.trim();
	if (key.length > 500)
		throw new Error("KMS key must be at most 500 characters.");
	return key ? { Enabled: true, KmsKeyId: key } : { Enabled: true };
}

function endpointOptions(f: DomainForm): DomainEndpointOptions {
	if (!tlsPolicies.includes(f.tlsPolicy))
		throw new Error("Choose a TLS security policy.");
	return { EnforceHTTPS: f.enforceHTTPS, TLSSecurityPolicy: f.tlsPolicy };
}

// policyText checks an access policy and writes it on one line, as the service stores it. The empty text is no
// policy.
export function policyText(text: string) {
	const policy = text.trim();
	return policy ? JSON.stringify(jsonObject(policy, "Access policy")) : "";
}

// The groups of settings a domain has, and the fields of the form that make up each. Setting a group sends all of
// it, which is how the service tells that an optional member was dropped.
const groups = {
	ClusterConfig: {
		fields: [
			"instanceType",
			"instanceCount",
			"dedicatedMaster",
			"masterType",
			"masterCount",
			"zoneAwareness",
		],
		build: clusterConfig,
	},
	EBSOptions: {
		fields: ["ebsEnabled", "volumeType", "volumeSize", "iops", "throughput"],
		build: ebsOptions,
	},
	EncryptionAtRestOptions: {
		fields: ["atRest", "kmsKey"],
		build: encryptionAtRest,
	},
	NodeToNodeEncryptionOptions: {
		fields: ["nodeToNode"],
		build: (f: DomainForm): NodeToNodeEncryptionOptions => ({
			Enabled: f.nodeToNode,
		}),
	},
	DomainEndpointOptions: {
		fields: ["enforceHTTPS", "tlsPolicy"],
		build: endpointOptions,
	},
} satisfies Record<
	string,
	{ fields: (keyof DomainForm)[]; build: (f: DomainForm) => unknown }
>;

// createDomainInput builds CreateDomain from a complete form.
export function createDomainInput(f: DomainForm) {
	const name = f.name.trim();
	if (!domainName.test(name))
		throw new Error(
			"Domain name must be 3 to 28 characters: lowercase letters, digits and hyphens, beginning with a letter.",
		);
	if (!enginePattern.test(f.engineVersion))
		throw new Error("Choose an engine version.");
	const policy = policyText(f.policy);
	const tags = tagsFromRows(f.tags);
	return {
		DomainName: name,
		AdvancedOptions: advancedOptionsInput(f.advancedOptions),
		EngineVersion: f.engineVersion,
		ClusterConfig: groups.ClusterConfig.build(f),
		EBSOptions: groups.EBSOptions.build(f),
		EncryptionAtRestOptions: groups.EncryptionAtRestOptions.build(f),
		NodeToNodeEncryptionOptions: groups.NodeToNodeEncryptionOptions.build(f),
		DomainEndpointOptions: groups.DomainEndpointOptions.build(f),
		...(policy ? { AccessPolicies: policy } : {}),
		...(Object.keys(tags).length ? { TagList: tagList(tags) } : {}),
	};
}

// updateDomainInput builds UpdateDomainConfig from the groups of settings the form changed. A cleared policy is
// sent as the empty text, which removes it.
export function updateDomainInput(domain: DomainStatus, form: DomainForm) {
	const before = formFromDomain(domain);
	const input: Record<string, unknown> = { DomainName: domain.DomainName };
	for (const [name, group] of Object.entries(groups))
		if (group.fields.some((field) => form[field] !== before[field]))
			input[name] = group.build(form);
	if (input.EBSOptions && form.ebsEnabled) {
		const storage = input.EBSOptions as EBSOptions;
		if (domain.EBSOptions?.Iops !== undefined && !form.iops.trim())
			storage.Iops = 0;
		if (domain.EBSOptions?.Throughput !== undefined && !form.throughput.trim())
			storage.Throughput = 0;
	}

	if (form.advancedOptions !== before.advancedOptions)
		input.AdvancedOptions = advancedOptionsInput(form.advancedOptions);
	if (form.policy !== before.policy)
		input.AccessPolicies = policyText(form.policy);
	if (Object.keys(input).length === 1)
		throw new Error("Change at least one setting.");
	return input;
}

// Display ---------------------------------------------------------------------

export type DomainState = "active" | "processing" | "deleting" | "failed";

// domainState reads the lifecycle from the flags DescribeDomain gives. A domain that is not processing and has no
// endpoint never got its engine to answer: records domains always have an endpoint.
export function domainState(
	d: Pick<DomainStatus, "Deleted" | "Processing" | "Endpoint">,
): DomainState {
	if (d.Deleted) return "deleting";
	if (d.Processing) return "processing";
	return d.Endpoint ? "active" : "failed";
}

export const stateLabels: Record<DomainState, string> = {
	active: "Active",
	processing: "Processing",
	deleting: "Deleting",
	failed: "Failed",
};

export function stateType(state: DomainState) {
	switch (state) {
		case "active":
			return "success" as const;
		case "failed":
			return "error" as const;
		default:
			return "in-progress" as const;
	}
}

// hasPendingDomains reports whether a domain is still changing, so the page polls.
export function hasPendingDomains(
	domains: Pick<DomainStatus, "Deleted" | "Processing" | "Endpoint">[],
) {
	return domains.some((d) => {
		const state = domainState(d);
		return state === "processing" || state === "deleting";
	});
}

// configProcessing reports whether a configuration change is still being applied.
export function configProcessing(config?: DomainConfig) {
	return Object.values(config ?? {}).some(
		(option) =>
			option?.Status?.State === "Processing" ||
			!!option?.Status?.PendingDeletion,
	);
}

// optionStateType maps the state of one configuration option to a status indicator.
export function optionStateType(status?: OptionStatus) {
	if (status?.PendingDeletion) return "in-progress" as const;
	switch (status?.State) {
		case "Active":
			return "success" as const;
		case "Processing":
			return "in-progress" as const;
		case "RequiresIndexDocuments":
			return "warning" as const;
		default:
			return "stopped" as const;
	}
}

export type OptionRow = {
	id: ConfigOption;
	label: string;
	status?: OptionStatus;
};

// optionRows lists the status of every option, in the order the page shows them.
export function optionRows(config?: DomainConfig): OptionRow[] {
	return configOptions.map(([id, label]) => ({
		id,
		label,
		status: config?.[id]?.Status,
	}));
}

// creationDate is when the domain was made: the earliest time any of its options was.
export function creationDate(config?: DomainConfig) {
	const times = Object.values(config ?? {})
		.map((option) => option?.Status?.CreationDate)
		.filter((time): time is number => typeof time === "number" && time > 0);
	return times.length ? Math.min(...times) : undefined;
}

export function capacityText(c?: ClusterConfig) {
	if (c?.InstanceCount === undefined || !c.InstanceType) return "-";
	return `${c.InstanceCount} × ${c.InstanceType}`;
}

export function mastersText(c?: ClusterConfig) {
	if (!c?.DedicatedMasterEnabled) return "Not used";
	return `${c.DedicatedMasterCount ?? "-"} × ${c.DedicatedMasterType ?? "-"}`;
}

// valueText shows a number or text, and a dash when the domain has none.
export const valueText = (value?: number | string, unit = "") =>
	value === undefined || value === "" ? "-" : `${value}${unit}`;

export const enabledText = (on?: boolean) => (on ? "Enabled" : "Disabled");

// The runtime shapes what an endpoint is, so the page says which one it runs.
export const runtimeHeader = (runtime?: string) =>
	runtime === "docker" ? "Docker runtime configured" : "Records-only runtime";

export const runtimeNote = (runtime?: string) =>
	runtime === "docker"
		? "Each domain runs one single-node OpenSearch or Elasticsearch engine in Docker. A new domain is processing until its engine answers, and the page refreshes the status while a domain changes. Instance, storage and security settings are stored as configuration: they do not resize or secure the engine. Engine upgrades, upgrade checks and custom advanced options are unavailable in Docker mode."
		: "Domains are stored as records. No search engine runs, so the endpoint is a placeholder name that nothing listens on. Set opensearch.domains to docker for a working engine.";

// connectCommand shows how to reach a Docker domain's engine, which answers plain HTTP on a loopback port. A
// records domain has nothing to reach.
export function connectCommand(runtime?: string, endpoint?: string) {
	return runtime === "docker" && endpoint
		? `curl http://${endpoint}/_cluster/health?pretty`
		: "";
}

// inventoryFailure is why the Docker runtime could not start a domain, which only the inventory reports.
export function inventoryFailure(
	resources: { arn: string; details?: Record<string, unknown> }[],
	arn?: string,
) {
	if (!arn) return "";
	const failure = resources.find((r) => r.arn === arn)?.details?.Failure;
	return typeof failure === "string" ? failure : "";
}
