import { stringMap } from "./consoleActions.ts";

export type DSQLScope = { account: string; region: string };

export type Cluster = {
	Identifier: string;
	Arn: string;
	Status?: string;
	// CreationTime is in Unix seconds.
	CreationTime?: number;
	DeletionProtectionEnabled?: boolean;
	Endpoint?: string;
	Tags?: Record<string, string>;
	MultiRegionProperties?: { WitnessRegion?: string; Clusters?: string[] };
	EncryptionDetails?: {
		EncryptionType?: string;
		EncryptionStatus?: string;
	};
};

// pending reports a status that is still changing, or waits for a peer cluster, so
// the page keeps refreshing until it settles.
export const pending = (status?: string) =>
	[
		"CREATING",
		"UPDATING",
		"DELETING",
		"PENDING_SETUP",
		"PENDING_DELETE",
	].includes(status ?? "");

// editable reports the statuses in which the service accepts UpdateCluster and DeleteCluster.
export const editable = (status?: string) =>
	status === "ACTIVE" || status === "PENDING_SETUP";

export function statusType(status?: string) {
	switch (status) {
		case "ACTIVE":
			return "success";
		case "CREATING":
		case "UPDATING":
		case "DELETING":
			return "in-progress";
		case "PENDING_SETUP":
		case "PENDING_DELETE":
			return "pending";
		default:
			return "info";
	}
}

export function dsqlHref(s: DSQLScope, cluster = "", tab = "overview") {
	const params = new URLSearchParams({ ...s, tab });
	if (cluster) params.set("cluster", cluster);
	return `/services/dsql?${params}`;
}

// A multi-Region cluster pairs two Regions of one set, each paired with a witness
// Region of the same set. The service checks the same sets, so this list only
// narrows the choices offered.
const regionSets = [
	["us-east-1", "us-east-2", "us-west-2", "ca-central-1", "ca-west-1"],
	[
		"ap-east-1",
		"ap-south-1",
		"ap-northeast-1",
		"ap-northeast-2",
		"ap-northeast-3",
		"ap-southeast-1",
	],
	[
		"eu-central-1",
		"eu-west-1",
		"eu-west-2",
		"eu-west-3",
		"eu-south-2",
		"eu-north-1",
	],
];

// witnessRegions lists the Regions that can witness a multi-Region cluster
// created in region. It is empty for a Region outside every set.
export const witnessRegions = (region: string) =>
	(regionSets.find((set) => set.includes(region)) ?? []).filter(
		(r) => r !== region,
	);

// peerArns lists the other clusters a multi-Region cluster is paired with. The
// service includes the cluster itself in the list it returns.
export const peerArns = (c: Cluster) =>
	(c.MultiRegionProperties?.Clusters ?? []).filter((arn) => arn !== c.Arn);

// needsPeer reports a multi-Region cluster that is still waiting to be paired.
export const needsPeer = (c: Cluster) =>
	!!c.MultiRegionProperties && peerArns(c).length === 0;

const clusterArn =
	/^arn:aws:dsql:[a-z0-9-]{1,20}:\d{12}:cluster\/[a-z0-9]{26}$/;

function peerArn(value: string) {
	const arn = value.trim();
	if (!clusterArn.test(arn))
		throw new Error(
			"Enter the peer cluster's ARN, such as arn:aws:dsql:us-east-2:123456789012:cluster/abcdefghijklmnopqrstuvwxyz.",
		);
	return arn;
}

export type CreateForm = {
	protection: boolean;
	tags: string;
	multi: boolean;
	witness: string;
	peer: string;
};

export function createInput(f: CreateForm, region: string) {
	const tags = stringMap(f.tags, "Tags");
	const input: Record<string, unknown> = {
		DeletionProtectionEnabled: f.protection,
		// An empty tag map is left out rather than sent.
		...(Object.keys(tags).length ? { Tags: tags } : {}),
	};
	if (f.multi) {
		if (!witnessRegions(region).includes(f.witness))
			throw new Error(
				`Choose a witness Region in the same Region set as ${region}.`,
			);
		input.MultiRegionProperties = {
			WitnessRegion: f.witness,
			...(f.peer.trim() ? { Clusters: [peerArn(f.peer)] } : {}),
		};
	}
	return input;
}

// updateInput sends only what changed. The peer is entered once, to pair a
// multi-Region cluster; the service keeps the witness Region it already has.
export function updateInput(
	c: Cluster,
	f: { protection: boolean; peer: string },
) {
	const input: Record<string, unknown> = { Identifier: c.Identifier };
	if (f.protection !== !!c.DeletionProtectionEnabled)
		input.DeletionProtectionEnabled = f.protection;
	if (needsPeer(c) && f.peer.trim())
		input.MultiRegionProperties = { Clusters: [peerArn(f.peer)] };
	if (Object.keys(input).length === 1)
		throw new Error("Change at least one setting.");
	return input;
}

// proxyPort finds the loopback port of a Docker cluster's TLS proxy in the
// inventory, where the service reports it. A cluster without a running proxy has none.
export function proxyPort(
	resources: { arn: string; details?: Record<string, unknown> }[],
	arn: string,
) {
	const address = resources.find((r) => r.arn === arn)?.details?.Address;
	const port =
		typeof address === "string"
			? Number(/^127\.0\.0\.1:(\d+)$/.exec(address)?.[1])
			: 0;
	return port > 0 && port < 65536 ? port : undefined;
}

// Only plain host names go into the commands below, so a value the server sends
// can never add shell or connection-string syntax to a command people copy.
const hostname = /^[a-z0-9][a-z0-9.-]*$/;

// psqlCommand connects through the loopback proxy of a Docker cluster. The cluster
// endpoint is the name the certificate is verified against; the loopback address
// carries the traffic.
export function psqlCommand(endpoint: string, port: number) {
	if (!hostname.test(endpoint) || !Number.isInteger(port)) return "";
	if (port < 1 || port > 65535) return "";
	return `psql "host=${endpoint} hostaddr=127.0.0.1 port=${port} dbname=postgres user=admin sslmode=verify-full sslrootcert=tinystack-ca.pem"`;
}

// tokenCommand makes an admin auth token, the password for the admin role.
export function tokenCommand(endpoint: string, region: string) {
	if (!hostname.test(endpoint) || !hostname.test(region)) return "";
	return `export PGPASSWORD="$(aws dsql generate-db-connect-admin-auth-token --hostname ${endpoint} --region ${region})"`;
}

// caCommand writes the certificate authority that signs the proxy's certificate.
export const caCommand =
	"tinystack ca export --config tinystack.yaml --out tinystack-ca.pem";
