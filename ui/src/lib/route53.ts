import { api } from "./api";
import { bareZoneId, type RecordSet } from "./route53Helpers";

// Route 53 is global, so a scope has an account and no Region.
export type Route53Scope = { account: string; zone?: string };
export type HostedZone = {
	Id: string;
	Name: string;
	CallerReference?: string;
	Config?: { Comment?: string; PrivateZone?: boolean };
	ResourceRecordSetCount?: number;
};
export type HealthCheck = {
	Id: string;
	HealthCheckVersion?: number;
	HealthCheckConfig: {
		Type: string;
		IPAddress?: string;
		FullyQualifiedDomainName?: string;
		Port?: number;
		ResourcePath?: string;
		SearchString?: string;
		RequestInterval?: number;
		FailureThreshold?: number;
	};
};
export type Route53Tag = { Key: string; Value: string };

export function route53Href(s: Route53Scope) {
	const params = new URLSearchParams({ account: s.account });
	if (s.zone) params.set("zone", s.zone);
	return `/services/route53?${params}`;
}
export function route53Call<T = Record<string, unknown>>(
	s: Route53Scope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("route53", operation, s.account, input);
}
// route53Pages follows Marker/NextMarker pages of a list operation.
export async function route53Pages<T>(
	s: Route53Scope,
	operation: string,
	field: string,
): Promise<T[]> {
	const items: T[] = [],
		seen = new Set<string>();
	let marker: string | undefined;
	do {
		const out = await route53Call<Record<string, unknown>>(
			s,
			operation,
			marker ? { Marker: marker } : {},
		);
		items.push(...((out[field] as T[] | undefined) ?? []));
		marker = out.IsTruncated
			? (out.NextMarker as string | undefined)
			: undefined;
		if (marker && seen.has(marker))
			throw new Error("The server returned an invalid page marker.");
		if (marker) seen.add(marker);
	} while (marker);
	return items;
}
export async function recordPages(
	s: Route53Scope,
	zone: string,
): Promise<RecordSet[]> {
	const items: RecordSet[] = [],
		seen = new Set<string>();
	let start: Record<string, string> = {};
	do {
		const out = await route53Call<{
			ResourceRecordSets?: RecordSet[];
			IsTruncated?: boolean;
			NextRecordName?: string;
			NextRecordType?: string;
			NextRecordIdentifier?: string;
		}>(s, "ListResourceRecordSets", {
			HostedZoneId: bareZoneId(zone),
			...start,
		});
		items.push(...(out.ResourceRecordSets ?? []));
		start = {};
		if (out.IsTruncated && out.NextRecordName) {
			start = {
				StartRecordName: out.NextRecordName,
				...(out.NextRecordType ? { StartRecordType: out.NextRecordType } : {}),
				...(out.NextRecordIdentifier
					? { StartRecordIdentifier: out.NextRecordIdentifier }
					: {}),
			};
			const token = JSON.stringify(start);
			if (seen.has(token))
				throw new Error("The server returned an invalid page marker.");
			seen.add(token);
		}
	} while (start.StartRecordName);
	return items;
}
