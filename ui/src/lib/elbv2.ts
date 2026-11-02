import { api } from "./api";
import type { Action, Condition } from "./elbv2RuleHelpers";

export type ELBv2Scope = { account: string; region: string };
export type Attribute = { Key: string; Value?: string };
export type Tag = { Key: string; Value?: string };
export type LoadBalancer = {
	LoadBalancerArn: string;
	LoadBalancerName: string;
	DNSName?: string;
	Scheme?: string;
	Type?: string;
	VpcId?: string;
	IpAddressType?: string;
	State?: { Code?: string; Reason?: string };
	AvailabilityZones?: { ZoneName?: string; SubnetId?: string }[];
	SecurityGroups?: string[];
};
export type Matcher = { HttpCode?: string; GrpcCode?: string };
export type TargetGroup = {
	TargetGroupArn: string;
	TargetGroupName: string;
	Protocol?: string;
	Port?: number;
	VpcId?: string;
	TargetType?: string;
	ProtocolVersion?: string;
	HealthCheckEnabled?: boolean;
	HealthCheckProtocol?: string;
	HealthCheckPort?: string;
	HealthCheckPath?: string;
	HealthCheckIntervalSeconds?: number;
	HealthCheckTimeoutSeconds?: number;
	HealthyThresholdCount?: number;
	UnhealthyThresholdCount?: number;
	Matcher?: Matcher;
	LoadBalancerArns?: string[];
};
export type Certificate = { CertificateArn?: string; IsDefault?: boolean };
export type Listener = {
	ListenerArn: string;
	LoadBalancerArn?: string;
	Protocol?: string;
	Port?: number;
	SslPolicy?: string;
	Certificates?: Certificate[];
	DefaultActions?: Action[];
};
export type Rule = {
	RuleArn: string;
	Priority?: string;
	IsDefault?: boolean;
	Conditions?: Condition[];
	Actions?: Action[];
};
export type TargetHealth = {
	Target?: { Id?: string; Port?: number; AvailabilityZone?: string };
	HealthCheckPort?: string;
	TargetHealth?: { State?: string; Reason?: string; Description?: string };
};
export type SslPolicy = {
	Name?: string;
	SupportedLoadBalancerTypes?: string[];
};
export type Subnet = {
	SubnetId?: string;
	VpcId?: string;
	AvailabilityZone?: string;
	CidrBlock?: string;
};
export type SecurityGroup = {
	GroupId?: string;
	GroupName?: string;
	VpcId?: string;
};
export type Vpc = { VpcId?: string; CidrBlock?: string };
export type Instance = {
	InstanceId?: string;
	VpcId?: string;
	State?: { Name?: string };
};

export function elbv2Href(s: ELBv2Scope) {
	return `/services/elbv2?${new URLSearchParams({ account: s.account, region: s.region })}`;
}

export function elbv2Call<T = Record<string, unknown>>(
	s: ELBv2Scope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("elbv2", operation, s.account, input, s.region);
}

// elbv2Pages follows NextMarker until the service stops returning one.
export async function elbv2Pages<T>(
	s: ELBv2Scope,
	operation: string,
	key: string,
	input: Record<string, unknown> = {},
): Promise<T[]> {
	const items: T[] = [],
		seen = new Set<string>();
	let marker: string | undefined;
	do {
		const out = await elbv2Call<Record<string, unknown>>(s, operation, {
			...input,
			PageSize: 400,
			...(marker ? { Marker: marker } : {}),
		});
		items.push(...((out[key] as T[] | undefined) ?? []));
		marker = out.NextMarker as string | undefined;
		if (marker && seen.has(marker))
			throw new Error("The server returned a repeated page marker.");
		if (marker) seen.add(marker);
	} while (marker);
	return items;
}

export const listLoadBalancers = (s: ELBv2Scope) =>
	elbv2Pages<LoadBalancer>(s, "DescribeLoadBalancers", "LoadBalancers");
export const listTargetGroups = (s: ELBv2Scope, input = {}) =>
	elbv2Pages<TargetGroup>(s, "DescribeTargetGroups", "TargetGroups", input);
export const listListeners = (s: ELBv2Scope, lbArn: string) =>
	elbv2Pages<Listener>(s, "DescribeListeners", "Listeners", {
		LoadBalancerArn: lbArn,
	});
export const listRules = (s: ELBv2Scope, listenerArn: string) =>
	elbv2Pages<Rule>(s, "DescribeRules", "Rules", { ListenerArn: listenerArn });
export const listListenerCertificates = (s: ELBv2Scope, listenerArn: string) =>
	elbv2Pages<Certificate>(s, "DescribeListenerCertificates", "Certificates", {
		ListenerArn: listenerArn,
	});
export const listSslPolicies = (s: ELBv2Scope) =>
	elbv2Pages<SslPolicy>(s, "DescribeSSLPolicies", "SslPolicies");

export async function describeAttributes(
	s: ELBv2Scope,
	operation: string,
	key: string,
	arn: string,
): Promise<Attribute[]> {
	const out = await elbv2Call<{ Attributes?: Attribute[] }>(s, operation, {
		[key]: arn,
	});
	return out.Attributes ?? [];
}

export async function describeTags(
	s: ELBv2Scope,
	arn: string,
): Promise<Record<string, string>> {
	const out = await elbv2Call<{
		TagDescriptions?: { Tags?: Tag[] }[];
	}>(s, "DescribeTags", { ResourceArns: [arn] });
	return Object.fromEntries(
		(out.TagDescriptions?.[0]?.Tags ?? []).map((t) => [t.Key, t.Value ?? ""]),
	);
}

async function ec2<T>(
	s: ELBv2Scope,
	operation: string,
	key: string,
): Promise<T[]> {
	const out = await api.action<Record<string, T[] | undefined>>(
		"ec2",
		operation,
		s.account,
		{},
		s.region,
	);
	return out[key] ?? [];
}

export const listSubnets = (s: ELBv2Scope) =>
	ec2<Subnet>(s, "DescribeSubnets", "Subnets");
export const listSecurityGroups = (s: ELBv2Scope) =>
	ec2<SecurityGroup>(s, "DescribeSecurityGroups", "SecurityGroups");
export const listVpcs = (s: ELBv2Scope) => ec2<Vpc>(s, "DescribeVpcs", "Vpcs");

export async function listInstances(s: ELBv2Scope): Promise<Instance[]> {
	const out = await api.action<{
		Reservations?: { Instances?: Instance[] }[];
	}>("ec2", "DescribeInstances", s.account, {}, s.region);
	return (out.Reservations ?? []).flatMap((r) => r.Instances ?? []);
}
