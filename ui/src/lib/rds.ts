import { api } from "./api";
export type RDSScope = {
	account: string;
	region: string;
	database?: string;
	section?: string;
};
export type Tag = { Key: string; Value: string };
export type DBInstance = {
	DBInstanceIdentifier: string;
	DBClusterIdentifier?: string;
	DBInstanceArn: string;
	Engine: string;
	EngineVersion: string;
	DBInstanceStatus: string;
	DBInstanceClass: string;
	MasterUsername: string;
	DBName?: string;
	AllocatedStorage?: number;
	InstanceCreateTime?: string | number;
	Endpoint?: { Address: string; Port: number };
	DBSubnetGroup?: { DBSubnetGroupName: string };
	DBParameterGroups?: { DBParameterGroupName: string }[];
	TagList?: Tag[];
};
export type DBGroup = {
	DBSubnetGroupName?: string;
	DBSubnetGroupDescription?: string;
	VpcId?: string;
	Subnets?: { SubnetIdentifier: string }[];
	DBParameterGroupName?: string;
	DBClusterParameterGroupName?: string;
	DBParameterGroupFamily?: string;
	Description?: string;
};
export type DBParameter = {
	ParameterName: string;
	ParameterValue?: string;
	AllowedValues?: string;
	Source?: string;
	ApplyType?: string;
	DataType?: string;
	IsModifiable?: boolean;
};
export const engines = [
	{
		value: "postgres",
		label: "PostgreSQL",
		versions: ["14", "15", "16", "17"],
	},
	{ value: "mysql", label: "MySQL", versions: ["8.0"] },
	{ value: "mariadb", label: "MariaDB", versions: ["11"] },
];
export const parameterFamilies = [
	...engines.flatMap((e) => e.versions.map((v) => e.value + v)),
	"neptune1.2",
	"neptune1.3",
];
export const clusterParameterFamilies = [
	"aurora-postgresql15",
	"aurora-postgresql16",
	"aurora-postgresql17",
	"aurora-mysql8.0",
	"docdb3.6",
	"docdb4.0",
	"docdb5.0",
	"neptune1.2",
	"neptune1.3",
];
export function rdsHref(s: RDSScope) {
	const p = new URLSearchParams({ account: s.account, region: s.region });
	if (s.database) p.set("database", s.database);
	if (s.section) p.set("section", s.section);
	return `/services/rds?${p}`;
}
export function rdsCall<T = Record<string, unknown>>(
	s: RDSScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("rds", operation, s.account, input, s.region);
}
export async function rdsPages<T>(
	s: RDSScope,
	operation: string,
	key: string,
	input: Record<string, unknown> = {},
): Promise<T[]> {
	const items: T[] = [],
		seen = new Set<string>();
	let marker: string | undefined;
	do {
		const out = await rdsCall<Record<string, unknown>>(s, operation, {
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
export const runtimeNote =
	"SQL connections require rds.instances: docker and a working local Docker daemon. In records mode, no database listens on the endpoint. Class and storage are metadata; parameter group values apply only to Docker-backed databases.";

export function rdsDate(value?: string | number) {
	if (value === undefined) return "—";
	const date = new Date(typeof value === "number" ? value * 1000 : value);
	return Number.isNaN(date.getTime()) ? "—" : date.toISOString();
}

export type DBCluster = {
	DBClusterIdentifier: string;
	DBClusterArn: string;
	Engine: string;
	EngineVersion: string;
	Status: string;
	Endpoint?: string;
	ReaderEndpoint?: string;
	Port?: number;
	BackupRetentionPeriod?: number;
	DeletionProtection?: boolean;
	HttpEndpointEnabled?: boolean;
	DBClusterMembers?: {
		DBInstanceIdentifier: string;
		IsClusterWriter: boolean;
	}[];
	TagList?: Tag[];
};
export type DBSnapshot = {
	DBSnapshotIdentifier?: string;
	DBClusterSnapshotIdentifier?: string;
	DBSnapshotArn?: string;
	DBClusterSnapshotArn?: string;
	DBInstanceIdentifier?: string;
	DBClusterIdentifier?: string;
	Engine: string;
	EngineVersion: string;
	Status: string;
	SnapshotCreateTime?: string | number;
	TagList?: Tag[];
};
export const clusterEngines = [
	"aurora-postgresql",
	"aurora-mysql",
	"docdb",
	"neptune",
];

export {
	finalSnapshotInput,
	hasPendingSnapshots,
	validateName,
	validatePassword,
} from "./rdsHelpers";
