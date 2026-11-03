import { integer, stringMap, words } from "./consoleActions.ts";
export type KafkaScope = { account: string; region: string };
export type Cluster = {
	ClusterArn: string;
	ClusterName: string;
	CreationTime?: string;
	CurrentVersion?: string;
	State?: string;
	StateInfo?: { Code?: string; Message?: string };
	CurrentBrokerSoftwareInfo?: {
		KafkaVersion?: string;
		ConfigurationArn?: string;
		ConfigurationRevision?: number;
	};
	NumberOfBrokerNodes?: number;
	EnhancedMonitoring?: string;
	Tags?: Record<string, string>;
	BrokerNodeGroupInfo?: {
		ClientSubnets?: string[];
		SecurityGroups?: string[];
		InstanceType?: string;
		StorageInfo?: { EbsStorageInfo?: { VolumeSize?: number } };
	};
	ClientAuthentication?: Record<string, unknown>;
	EncryptionInfo?: Record<string, unknown>;
};
export type Node = {
	NodeType?: string;
	InstanceType?: string;
	AddedToClusterTime?: string;
	BrokerNodeInfo?: {
		BrokerId?: number;
		ClientSubnet?: string;
		Endpoints?: string[];
	};
};
export const versions = ["3.8.x", "3.9.x"];
export const instances = ["kafka.m5.large", "kafka.m5.xlarge"];
export const monitoring = [
	"DEFAULT",
	"PER_BROKER",
	"PER_TOPIC_PER_BROKER",
	"PER_TOPIC_PER_PARTITION",
];
export const createRegions = ["us-east-1", "us-west-1", "us-west-2"];
export const pending = (state?: string) =>
	[
		"CREATING",
		"UPDATING",
		"DELETING",
		"HEALING",
		"MAINTENANCE",
		"REBOOTING_BROKER",
	].includes(state ?? "");
export function kafkaHref(s: KafkaScope, cluster = "", tab = "overview") {
	const params = new URLSearchParams({ ...s, tab });
	if (cluster) params.set("cluster", cluster);
	return `/services/kafka?${params}`;
}
export type ClusterForm = {
	name: string;
	version: string;
	count: string;
	instance: string;
	subnets: string[];
	groups: string[];
	storage: string;
	monitoring: string;
	tags: string;
	auth: string;
	encryption: string;
	inCluster: boolean;
	authorities: string;
};
export function clusterInput(f: ClusterForm, region: string, docker: boolean) {
	if (!createRegions.includes(region))
		throw new Error("Create clusters in us-east-1, us-west-1 or us-west-2.");
	if (!/^[A-Za-z0-9_-]{1,64}$/.test(f.name.trim()))
		throw new Error(
			"Name requires 1–64 letters, digits, underscores or hyphens.",
		);
	if (!versions.includes(f.version) || !instances.includes(f.instance))
		throw new Error("Choose a supported Kafka version and instance type.");
	if (
		f.subnets.length < 2 ||
		f.subnets.length > 3 ||
		(region === "us-west-1" && f.subnets.length !== 2) ||
		new Set(f.subnets).size !== f.subnets.length
	)
		throw new Error(
			"Choose two or three distinct subnets (exactly two in us-west-1).",
		);
	if (
		!f.groups.length ||
		f.groups.length > 5 ||
		new Set(f.groups).size !== f.groups.length
	)
		throw new Error("Choose one to five distinct security groups.");
	const count = integer(f.count, "Broker count", 1, 15);
	if (count % f.subnets.length)
		throw new Error("Broker count must be a multiple of the subnet count.");
	if (!monitoring.includes(f.monitoring))
		throw new Error("Choose a supported monitoring level.");
	if (!["TLS", "PLAINTEXT", "TLS_PLAINTEXT"].includes(f.encryption))
		throw new Error("Choose client encryption.");
	if (!["Unauthenticated", "IAM", "SCRAM", "mTLS"].includes(f.auth))
		throw new Error("Choose client authentication.");
	if (docker && f.auth !== "Unauthenticated")
		throw new Error("Docker supports unauthenticated listeners only.");
	if (f.auth !== "Unauthenticated" && (f.encryption !== "TLS" || !f.inCluster))
		throw new Error(
			"Authenticated listeners require TLS and in-cluster encryption.",
		);
	const auth =
		f.auth === "Unauthenticated"
			? { Unauthenticated: { Enabled: true } }
			: f.auth === "mTLS"
				? {
						Tls: {
							Enabled: true,
							CertificateAuthorityArnList: words(f.authorities),
						},
					}
				: { Sasl: { [f.auth === "IAM" ? "Iam" : "Scram"]: { Enabled: true } } };
	return {
		ClusterName: f.name.trim(),
		KafkaVersion: f.version,
		NumberOfBrokerNodes: count,
		BrokerNodeGroupInfo: {
			InstanceType: f.instance,
			ClientSubnets: f.subnets,
			SecurityGroups: f.groups,
			StorageInfo: {
				EbsStorageInfo: {
					VolumeSize: integer(f.storage, "Storage (GiB)", 1, 16384),
				},
			},
		},
		EnhancedMonitoring: f.monitoring,
		Tags: stringMap(f.tags, "Tags"),
		ClientAuthentication: auth,
		EncryptionInfo: {
			EncryptionInTransit: {
				ClientBroker: f.encryption,
				InCluster: f.inCluster,
			},
		},
	};
}

export type Configuration = {
	Arn: string;
	Name: string;
	Description?: string;
	CreationTime?: string;
	KafkaVersions?: string[];
	State?: string;
	LatestRevision?: ConfigurationRevision;
};
export type ConfigurationRevision = {
	Revision: number;
	CreationTime?: string;
	Description?: string;
};
export function configurationInput(
	name: string,
	description: string,
	properties: string,
) {
	if (!/^[A-Za-z0-9][A-Za-z0-9-]*$/.test(name))
		throw new Error(
			"Choose a configuration name containing letters, digits or hyphens, starting with a letter or digit.",
		);
	if (!properties.trim()) throw new Error("Enter server.properties text.");
	return {
		Name: name,
		Description: description,
		ServerProperties: btoa(
			String.fromCharCode(...new TextEncoder().encode(properties)),
		),
	};
}
export function clusterUpdateInput(
	cluster: Cluster,
	action: string,
	value: string,
	revision = "",
) {
	const base = {
		ClusterArn: cluster.ClusterArn,
		CurrentVersion: cluster.CurrentVersion,
	};
	if (action === "count") {
		const count = integer(value, "Broker count", 1, 15);
		if (
			count <= (cluster.NumberOfBrokerNodes ?? 0) ||
			count % (cluster.BrokerNodeGroupInfo?.ClientSubnets?.length ?? 1)
		)
			throw new Error(
				"Broker count must increase and be a multiple of subnet count.",
			);
		return { ...base, TargetNumberOfBrokerNodes: count };
	}
	if (action === "storage") {
		const size = integer(value, "Storage (GiB)", 101, 16384);
		if (
			size <=
			(cluster.BrokerNodeGroupInfo?.StorageInfo?.EbsStorageInfo?.VolumeSize ??
				0)
		)
			throw new Error("Storage must increase.");
		return {
			...base,
			TargetBrokerEBSVolumeInfo: [
				{ KafkaBrokerNodeId: "ALL", VolumeSizeGB: size },
			],
		};
	}
	if (!value.trim()) throw new Error("Enter a configuration ARN.");
	return {
		...base,
		ConfigurationInfo: {
			Arn: value.trim(),
			Revision: integer(revision, "Revision", 1, Number.MAX_SAFE_INTEGER),
		},
	};
}
