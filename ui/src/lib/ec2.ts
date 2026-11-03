import { api, type Resource } from "./api";
import type { ActionStep } from "./consoleActions";
import { tagList } from "./tags.ts";

export const ec2Sections = [
	"instances",
	"images",
	"keys",
	"vpcs",
	"subnets",
	"groups",
	"routes",
	"gateways",
	"addresses",
	"nat",
	"acls",
	"endpoints",
	"interfaces",
] as const;
export type Ec2Section = (typeof ec2Sections)[number];
export type Ec2Scope = {
	account: string;
	region: string;
	area: "ec2" | "vpc";
	section: Ec2Section;
	id?: string;
	vpc?: string;
};
export type Ec2Row = Record<string, unknown>;
export type Ec2Kind = {
	title: string;
	singular: string;
	type: string;
	id: string;
	operation: string;
	key: string;
	ids?: string;
	paged?: boolean;
	create?: string;
	delete?: string;
	columns: [string, string][];
};
export const ec2Kinds: Record<Ec2Section, Ec2Kind> = {
	instances: {
		title: "Instances",
		singular: "Instance",
		type: "instance",
		id: "InstanceId",
		operation: "DescribeInstances",
		key: "Reservations",
		ids: "InstanceIds",
		paged: true,
		create: "RunInstances",
		delete: "TerminateInstances",
		columns: [
			["InstanceType", "Instance type"],
			["PrivateIpAddress", "Private IPv4"],
			["PublicIpAddress", "Public IPv4"],
			["VpcId", "VPC"],
		],
	},
	images: {
		title: "AMIs",
		singular: "AMI",
		type: "image",
		id: "ImageId",
		operation: "DescribeImages",
		key: "Images",
		ids: "ImageIds",
		paged: true,
		columns: [
			["Architecture", "Architecture"],
			["Description", "Description"],
		],
	},
	keys: {
		title: "Key pairs",
		singular: "Key pair",
		type: "key-pair",
		id: "KeyPairId",
		operation: "DescribeKeyPairs",
		key: "KeyPairs",
		ids: "KeyPairIds",
		create: "CreateKeyPair",
		delete: "DeleteKeyPair",
		columns: [
			["KeyType", "Key type"],
			["KeyFingerprint", "Fingerprint"],
		],
	},
	vpcs: {
		title: "Your VPCs",
		singular: "VPC",
		type: "vpc",
		id: "VpcId",
		operation: "DescribeVpcs",
		key: "Vpcs",
		ids: "VpcIds",
		paged: true,
		create: "CreateVpc",
		delete: "DeleteVpc",
		columns: [
			["CidrBlock", "IPv4 CIDR"],
			["IsDefault", "Default VPC"],
			["InstanceTenancy", "Tenancy"],
		],
	},
	subnets: {
		title: "Subnets",
		singular: "Subnet",
		type: "subnet",
		id: "SubnetId",
		operation: "DescribeSubnets",
		key: "Subnets",
		ids: "SubnetIds",
		paged: true,
		create: "CreateSubnet",
		delete: "DeleteSubnet",
		columns: [
			["VpcId", "VPC"],
			["CidrBlock", "IPv4 CIDR"],
			["AvailabilityZone", "Availability Zone"],
			["AvailableIpAddressCount", "Available IPv4 addresses"],
		],
	},
	groups: {
		title: "Security groups",
		singular: "Security group",
		type: "security-group",
		id: "GroupId",
		operation: "DescribeSecurityGroups",
		key: "SecurityGroups",
		ids: "GroupIds",
		paged: true,
		create: "CreateSecurityGroup",
		delete: "DeleteSecurityGroup",
		columns: [
			["GroupName", "Group name"],
			["VpcId", "VPC"],
			["Description", "Description"],
		],
	},
	routes: {
		title: "Route tables",
		singular: "Route table",
		type: "route-table",
		id: "RouteTableId",
		operation: "DescribeRouteTables",
		key: "RouteTables",
		ids: "RouteTableIds",
		paged: true,
		create: "CreateRouteTable",
		delete: "DeleteRouteTable",
		columns: [["VpcId", "VPC"]],
	},
	gateways: {
		title: "Internet gateways",
		singular: "Internet gateway",
		type: "internet-gateway",
		id: "InternetGatewayId",
		operation: "DescribeInternetGateways",
		key: "InternetGateways",
		ids: "InternetGatewayIds",
		paged: true,
		create: "CreateInternetGateway",
		delete: "DeleteInternetGateway",
		columns: [],
	},
	addresses: {
		title: "Elastic IP addresses",
		singular: "Elastic IP address",
		type: "elastic-ip",
		id: "AllocationId",
		operation: "DescribeAddresses",
		key: "Addresses",
		ids: "AllocationIds",
		create: "AllocateAddress",
		delete: "ReleaseAddress",
		columns: [
			["PublicIp", "Public IPv4"],
			["InstanceId", "Instance"],
			["AssociationId", "Association"],
			["NetworkBorderGroup", "Network border group"],
		],
	},
	nat: {
		title: "NAT gateways",
		singular: "NAT gateway",
		type: "natgateway",
		id: "NatGatewayId",
		operation: "DescribeNatGateways",
		key: "NatGateways",
		ids: "NatGatewayIds",
		paged: true,
		create: "CreateNatGateway",
		delete: "DeleteNatGateway",
		columns: [
			["VpcId", "VPC"],
			["SubnetId", "Subnet"],
			["ConnectivityType", "Connectivity"],
		],
	},
	acls: {
		title: "Network ACLs",
		singular: "Network ACL",
		type: "network-acl",
		id: "NetworkAclId",
		operation: "DescribeNetworkAcls",
		key: "NetworkAcls",
		ids: "NetworkAclIds",
		paged: true,
		create: "CreateNetworkAcl",
		delete: "DeleteNetworkAcl",
		columns: [
			["VpcId", "VPC"],
			["IsDefault", "Default ACL"],
		],
	},
	endpoints: {
		title: "Endpoints",
		singular: "Endpoint",
		type: "vpc-endpoint",
		id: "VpcEndpointId",
		operation: "DescribeVpcEndpoints",
		key: "VpcEndpoints",
		ids: "VpcEndpointIds",
		paged: true,
		create: "CreateVpcEndpoint",
		delete: "DeleteVpcEndpoints",
		columns: [
			["VpcId", "VPC"],
			["VpcEndpointType", "Endpoint type"],
			["ServiceName", "Service name"],
		],
	},
	interfaces: {
		title: "Network interfaces",
		singular: "Network interface",
		type: "network-interface",
		id: "NetworkInterfaceId",
		operation: "DescribeNetworkInterfaces",
		key: "NetworkInterfaces",
		ids: "NetworkInterfaceIds",
		paged: true,
		columns: [
			["VpcId", "VPC"],
			["SubnetId", "Subnet"],
			["PrivateIpAddress", "Private IPv4"],
			["InterfaceType", "Type"],
		],
	},
};
export function ec2Search(
	search: Record<string, unknown>,
	area: "ec2" | "vpc",
) {
	const section = String(
		search.section ?? (area === "vpc" ? "vpcs" : "instances"),
	);
	return {
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
		section: ec2Sections.includes(section as Ec2Section)
			? (section as Ec2Section)
			: area === "vpc"
				? ("vpcs" as const)
				: ("instances" as const),
		id: String(search.id ?? ""),
		vpc: String(search.vpc ?? ""),
	};
}
export function ec2Href(s: Ec2Scope) {
	const p = new URLSearchParams({
		account: s.account,
		region: s.region,
		section: s.section,
	});
	if (s.id) p.set("id", s.id);
	if (s.vpc) p.set("vpc", s.vpc);
	return `/services/${s.area}?${p}`;
}
export const text = (r: Ec2Row, k: string) => String(r[k] ?? "");
export const object = (r: Ec2Row, k: string): Ec2Row =>
	r[k] && typeof r[k] === "object" && !Array.isArray(r[k])
		? (r[k] as Ec2Row)
		: {};
export const rows = (r: Ec2Row, k: string): Ec2Row[] =>
	Array.isArray(r[k]) ? (r[k] as Ec2Row[]) : [];
export const strings = (r: Ec2Row, k: string): string[] =>
	Array.isArray(r[k]) ? (r[k] as string[]) : [];
export const rowId = (section: Ec2Section, r: Ec2Row) =>
	text(r, ec2Kinds[section].id);
export const tagsOf = (r: Ec2Row) =>
	Object.fromEntries(
		rows(r, "Tags").map((t) => [text(t, "Key"), text(t, "Value")]),
	);
export const rowName = (r: Ec2Row) =>
	tagsOf(r).Name ||
	text(r, "GroupName") ||
	text(r, "KeyName") ||
	text(r, "Name");
export const rowState = (r: Ec2Row) =>
	text(object(r, "State"), "Name") ||
	(typeof r.State === "string" ? r.State : "") ||
	text(r, "Status");
export const parentVpc = (r: Ec2Row) =>
	text(r, "VpcId") || text(rows(r, "Attachments")[0] ?? {}, "VpcId");
export const tagSpecifications = (
	section: Ec2Section,
	tags: Record<string, string>,
) =>
	Object.keys(tags).length
		? [{ ResourceType: ec2Kinds[section].type, Tags: tagList(tags) }]
		: undefined;
export function ec2Call<T = Ec2Row>(
	s: Pick<Ec2Scope, "account" | "region">,
	operation: string,
	input: Ec2Row = {},
) {
	return api.action<T>("ec2", operation, s.account, input, s.region);
}
export async function ec2List(
	s: Pick<Ec2Scope, "account" | "region">,
	section: Ec2Section,
	input: Ec2Row = {},
) {
	const kind = ec2Kinds[section],
		out: Ec2Row[] = [];
	const ids = kind.ids ? input[kind.ids] : undefined;
	const targeted = Array.isArray(ids) && ids.length > 0;
	let NextToken: string | undefined;
	const seen = new Set<string>();
	do {
		const result = await ec2Call(s, kind.operation, {
			...(kind.paged && !targeted ? { MaxResults: 100 } : {}),
			...(section === "keys" ? { IncludePublicKey: true } : {}),
			...input,
			...(NextToken ? { NextToken } : {}),
		});
		const batch = rows(result, kind.key);
		out.push(
			...(section === "instances"
				? batch.flatMap((r) => rows(r, "Instances"))
				: batch),
		);
		NextToken = text(result, "NextToken") || undefined;
		if (NextToken) {
			if (seen.has(NextToken))
				throw new Error(
					"The server repeated a pagination token. Refresh and try again.",
				);
			seen.add(NextToken);
		}
	} while (NextToken);
	return out;
}
export async function ec2Describe(s: Ec2Scope) {
	const kind = ec2Kinds[s.section];
	const items = await ec2List(
		s,
		s.section,
		kind.ids && s.id ? { [kind.ids]: [s.id] } : {},
	);
	const found = items.find((r) => rowId(s.section, r) === s.id);
	if (!found)
		throw new Error(
			`${kind.singular} ${s.id} was not found in this account and Region.`,
		);
	return found;
}
export function inventoryRow(r: Resource): Ec2Row {
	const section = ec2Sections.find((k) => ec2Kinds[k].type === r.type);
	if (!section) return {};
	const id = r.arn.split("/").at(-1) ?? r.name;
	return {
		[ec2Kinds[section].id]: id,
		Name: r.name,
		Tags: tagList(r.tags ?? {}),
		State: r.details?.State,
		VpcId: r.details?.VPCID,
	};
}
export function ec2TagPlan(
	s: Ec2Scope,
	tags: Record<string, string>,
	removed: string[],
): ActionStep[] {
	return [
		...(removed.length
			? [
					{
						label: "Remove tags",
						run: () =>
							ec2Call(s, "DeleteTags", {
								Resources: [s.id],
								Tags: removed.map((Key) => ({ Key })),
							}),
					},
				]
			: []),
		...(Object.keys(tags).length
			? [
					{
						label: "Save tags",
						run: () =>
							ec2Call(s, "CreateTags", {
								Resources: [s.id],
								Tags: tagList(tags),
							}),
					},
				]
			: []),
	];
}
export async function ec2Delete(s: Ec2Scope) {
	const kind = ec2Kinds[s.section];
	if (!kind.delete) throw new Error("This resource is read-only.");
	const out = await ec2Call(
		s,
		kind.delete,
		s.section === "instances"
			? { InstanceIds: [s.id] }
			: s.section === "endpoints"
				? { VpcEndpointIds: [s.id] }
				: { [kind.id]: s.id },
	);
	const failed = rows(out, "Unsuccessful");
	if (failed.length)
		throw new Error(
			failed
				.map(
					(r) =>
						`${text(r, "ResourceId")}: ${text(object(r, "Error"), "Message")}`,
				)
				.join("\n"),
		);
}
export const base64 = (text: string) =>
	btoa(String.fromCharCode(...new TextEncoder().encode(text)));
