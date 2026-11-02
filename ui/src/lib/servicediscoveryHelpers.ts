export type Pair = { key: string; value: string };
export type Namespace = {
	Id: string;
	Arn: string;
	Name: string;
	Type: string;
	Description?: string;
	ServiceCount?: number;
	CreateDate?: number;
};
export type DnsRecord = { Type: string; TTL: number };
export type Service = {
	Id: string;
	Arn: string;
	Name: string;
	Description?: string;
	InstanceCount?: number;
	CreateDate?: number;
	DnsConfig?: { RoutingPolicy?: string; DnsRecords: DnsRecord[] };
	HealthCheckCustomConfig?: { FailureThreshold?: number };
};
export type Instance = { Id: string; Attributes: Record<string, string> };
export type Operation = {
	Id: string;
	Type: string;
	Status: string;
	CreateDate?: number;
	Targets?: Record<string, string>;
};
export function pairMap(
	rows: Pair[],
	label = "Attributes",
): Record<string, string> {
	const out: Record<string, string> = Object.create(null);
	for (const { key, value } of rows) {
		if (!key.trim()) throw new Error(`${label} keys are required.`);
		if (Object.hasOwn(out, key))
			throw new Error(`${label} keys must be unique.`);
		out[key] = value;
	}
	return out;
}
export function tagList(rows: Pair[]) {
	return Object.entries(pairMap(rows, "Tags")).map(([Key, Value]) => ({
		Key,
		Value,
	}));
}
export function requiredName(value: string) {
	const name = value.trim();
	if (!name || name.length > 1024)
		throw new Error("Name is required and must be at most 1024 characters.");
	return name;
}
export function namespaceInput(
	name: string,
	description: string,
	type: string,
	vpc: string,
) {
	if (!["HTTP", "DNS_PRIVATE", "DNS_PUBLIC"].includes(type))
		throw new Error("Choose a namespace type.");
	if (type === "DNS_PRIVATE" && !vpc) throw new Error("Choose a VPC.");
	return {
		Name: requiredName(name),
		Description: description,
		...(type === "DNS_PRIVATE" ? { Vpc: vpc } : {}),
	};
}
export function dnsRecords(rows: { Type: string; TTL: string }[]): DnsRecord[] {
	if (!rows.length) throw new Error("At least one DNS record is required.");
	return rows.map(({ Type, TTL }) => {
		if (
			!["A", "AAAA", "SRV", "CNAME"].includes(Type) ||
			!TTL.trim() ||
			!Number.isSafeInteger(Number(TTL)) ||
			Number(TTL) < 0
		)
			throw new Error(
				"DNS records require a supported type and nonnegative integer TTL.",
			);
		return { Type, TTL: Number(TTL) };
	});
}
export function discoveryInput(
	namespace: string,
	service: string,
	query: Pair[],
	optional: Pair[],
	health: string,
) {
	if (!["HEALTHY", "UNHEALTHY", "ALL", "HEALTHY_OR_ELSE_ALL"].includes(health))
		throw new Error("Choose a health status.");
	return {
		NamespaceName: requiredName(namespace),
		ServiceName: requiredName(service),
		QueryParameters: pairMap(query),
		OptionalParameters: pairMap(optional),
		HealthStatus: health,
		MaxResults: 1000,
	};
}
