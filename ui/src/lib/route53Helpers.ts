export const recordTypes = [
	"A",
	"AAAA",
	"CNAME",
	"MX",
	"TXT",
	"NS",
	"SRV",
	"CAA",
	"PTR",
	"SPF",
	"NAPTR",
	"DS",
] as const;
export type RecordValue = { Value: string };
export type AliasTarget = {
	HostedZoneId: string;
	DNSName: string;
	EvaluateTargetHealth: boolean;
};
export type RecordSet = {
	Name: string;
	Type: string;
	SetIdentifier?: string;
	TTL?: number;
	ResourceRecords?: RecordValue[];
	AliasTarget?: AliasTarget;
	[routing: string]: unknown;
};
export type RecordForm = {
	name: string;
	type: string;
	ttl: string;
	values: string;
	alias: boolean;
	aliasDNSName: string;
	aliasZoneId: string;
	evaluateTargetHealth: boolean;
};
export type ChangeAction = "CREATE" | "UPSERT" | "DELETE";

export const emptyRecordForm: RecordForm = {
	name: "",
	type: "A",
	ttl: "300",
	values: "",
	alias: false,
	aliasDNSName: "",
	aliasZoneId: "",
	evaluateTargetHealth: false,
};

// bareZoneId drops the /hostedzone/ prefix the API puts on identifiers.
export function bareZoneId(id: string) {
	return id.replace(/^\/?hostedzone\//, "");
}

// withDot returns name as an absolute DNS name.
export function withDot(name: string) {
	return name.endsWith(".") ? name : `${name}.`;
}

// recordFullName turns what a person typed into the absolute name stored in
// the zone: empty or "@" is the apex, a name inside the zone is kept, and
// anything else is relative to the zone.
export function recordFullName(input: string, zoneName: string) {
	const zone = withDot(zoneName.trim().toLowerCase());
	const name = input.trim().toLowerCase();
	if (name === "" || name === "@") return zone;
	if (name.endsWith(".")) return name;
	if (`${name}.` === zone || name.endsWith(`.${zone.slice(0, -1)}`))
		return `${name}.`;
	return `${name}.${zone}`;
}

// relativeName shows a record name without its zone suffix.
export function relativeName(name: string, zoneName: string) {
	const zone = withDot(zoneName);
	if (name === zone) return "@";
	return name.endsWith(`.${zone}`) ? name.slice(0, -zone.length - 1) : name;
}

export function parseValues(text: string) {
	return text
		.split("\n")
		.map((line) => line.trim())
		.filter(Boolean);
}

export function formatValues(records: RecordValue[] | undefined) {
	return (records ?? []).map((r) => r.Value).join("\n");
}

// quoteText wraps a TXT or SPF value in the quotes the API requires, leaving a
// value that already is one or more quoted strings as it is.
export function quoteText(value: string) {
	if (/^("([^"\\]|\\.)*"\s*)+$/.test(value)) return value;
	return `"${value.replace(/\\/g, "\\\\").replace(/"/g, '\\"')}"`;
}

export function normalizeValues(type: string, values: string[]) {
	return type === "TXT" || type === "SPF" ? values.map(quoteText) : values;
}

export function recordTTL(text: string) {
	if (!/^\d+$/.test(text.trim()) || Number(text) > 2147483647)
		throw new Error("TTL must be a whole number from 0 to 2147483647.");
	return Number(text);
}

// buildRecordSet validates a form and returns the record set it describes.
// Editing keeps the name, type and routing fields of the record it replaces,
// since the form does not change them.
export function buildRecordSet(
	form: RecordForm,
	zoneName: string,
	existing?: RecordSet,
): RecordSet {
	if (!(recordTypes as readonly string[]).includes(form.type))
		throw new Error("Choose a record type.");
	const base: RecordSet = {
		Name: existing?.Name ?? recordFullName(form.name, zoneName),
		Type: existing?.Type ?? form.type,
	};
	for (const [key, value] of Object.entries(existing ?? {}))
		if (!["TTL", "ResourceRecords", "AliasTarget"].includes(key))
			base[key] = value;
	if (form.alias) {
		if (base.Type === "NS") throw new Error("NS records cannot be aliases.");
		if (!form.aliasDNSName.trim() || !form.aliasZoneId.trim())
			throw new Error("An alias needs a target DNS name and a hosted zone ID.");
		return {
			...base,
			AliasTarget: {
				DNSName: form.aliasDNSName.trim(),
				HostedZoneId: bareZoneId(form.aliasZoneId.trim()),
				EvaluateTargetHealth: form.evaluateTargetHealth,
			},
		};
	}
	const values = parseValues(form.values);
	if (!values.length) throw new Error("Enter at least one value.");
	if (base.Type === "CNAME" && values.length > 1)
		throw new Error("A CNAME record has exactly one value.");
	return {
		...base,
		TTL: recordTTL(form.ttl),
		ResourceRecords: normalizeValues(base.Type, values).map((Value) => ({
			Value,
		})),
	};
}

export function recordToForm(rs: RecordSet, zoneName: string): RecordForm {
	return {
		name: relativeName(rs.Name, zoneName),
		type: rs.Type,
		ttl: String(rs.TTL ?? 300),
		values: formatValues(rs.ResourceRecords),
		alias: !!rs.AliasTarget,
		aliasDNSName: rs.AliasTarget?.DNSName ?? "",
		aliasZoneId: rs.AliasTarget?.HostedZoneId ?? "",
		evaluateTargetHealth: !!rs.AliasTarget?.EvaluateTargetHealth,
	};
}

// changeBatch builds the ChangeBatch for one record set change.
export function changeBatch(
	action: ChangeAction,
	record: RecordSet,
	comment?: string,
) {
	return {
		...(comment ? { Comment: comment } : {}),
		Changes: [{ Action: action, ResourceRecordSet: record }],
	};
}

// isProtectedRecord is true for the zone's own SOA and NS record sets, which
// the service refuses to delete.
export function isProtectedRecord(rs: RecordSet, zoneName: string) {
	return (
		rs.Name === withDot(zoneName) && (rs.Type === "SOA" || rs.Type === "NS")
	);
}

export function recordTarget(rs: RecordSet) {
	if (rs.AliasTarget)
		return `ALIAS ${rs.AliasTarget.DNSName} (${rs.AliasTarget.HostedZoneId})`;
	return formatValues(rs.ResourceRecords);
}

export function recordKey(rs: RecordSet) {
	return `${rs.Name}|${rs.Type}|${rs.SetIdentifier ?? ""}`;
}

export const healthCheckTypes = [
	"HTTP",
	"HTTPS",
	"HTTP_STR_MATCH",
	"HTTPS_STR_MATCH",
	"TCP",
] as const;
export type HealthCheckForm = {
	type: string;
	ipAddress: string;
	domain: string;
	port: string;
	path: string;
	search: string;
	interval: string;
	threshold: string;
};
export const emptyHealthCheckForm: HealthCheckForm = {
	type: "HTTP",
	ipAddress: "",
	domain: "",
	port: "",
	path: "/",
	search: "",
	interval: "30",
	threshold: "3",
};

export function buildHealthCheckConfig(form: HealthCheckForm) {
	if (!(healthCheckTypes as readonly string[]).includes(form.type))
		throw new Error("Choose a health check type.");
	const ip = form.ipAddress.trim(),
		domain = form.domain.trim();
	if (!ip && !domain)
		throw new Error("Enter an IP address or a domain name to check.");
	if (form.port.trim() && !/^\d+$/.test(form.port.trim()))
		throw new Error("Port must be a number from 1 to 65535.");
	const port = form.port.trim() ? Number(form.port) : undefined;
	if (port !== undefined && (port < 1 || port > 65535))
		throw new Error("Port must be a number from 1 to 65535.");
	if (form.type === "TCP" && port === undefined)
		throw new Error("A TCP health check needs a port.");
	const match = form.type.endsWith("_STR_MATCH");
	if (match && !form.search)
		throw new Error("A string matching check needs a search string.");
	if (!/^(10|30)$/.test(form.interval))
		throw new Error("Request interval must be 10 or 30 seconds.");
	if (
		!/^\d+$/.test(form.threshold) ||
		Number(form.threshold) < 1 ||
		Number(form.threshold) > 10
	)
		throw new Error("Failure threshold must be a number from 1 to 10.");
	return {
		Type: form.type,
		...(ip ? { IPAddress: ip } : {}),
		...(domain ? { FullyQualifiedDomainName: domain } : {}),
		...(port !== undefined ? { Port: port } : {}),
		...(form.type !== "TCP" && form.path ? { ResourcePath: form.path } : {}),
		...(match ? { SearchString: form.search } : {}),
		RequestInterval: Number(form.interval),
		FailureThreshold: Number(form.threshold),
	};
}
