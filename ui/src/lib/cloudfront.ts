import { api } from "./api";
import type {
	DistributionConfig,
	OacConfig,
	OacForm,
	TagList,
} from "./cloudfrontHelpers";

// CloudFront is global, so a scope has an account and no Region.
export type CloudFrontScope = { account: string };
export type CloudFrontTab = "distributions" | "oacs" | "kvs";
export const cloudfrontTabs: CloudFrontTab[] = ["distributions", "oacs", "kvs"];

export type DistributionSummary = DistributionConfig & {
	Id: string;
	ARN: string;
	DomainName: string;
	Status: string;
	LastModifiedTime?: string;
};
export type Distribution = {
	Id: string;
	ARN: string;
	DomainName: string;
	Status: string;
	LastModifiedTime?: string;
	InProgressInvalidationBatches?: number;
	DistributionConfig?: DistributionConfig;
};
export type InvalidationSummary = {
	Id: string;
	Status: string;
	CreateTime?: string;
};
export type Invalidation = InvalidationSummary & {
	InvalidationBatch?: {
		CallerReference?: string;
		Paths?: { Items?: string[] };
	};
};
export type OacSummary = {
	Id: string;
	Name?: string;
	Description?: string;
	OriginAccessControlOriginType?: string;
	SigningBehavior?: string;
	SigningProtocol?: string;
};
export type KeyValueStore = {
	Name?: string;
	Id?: string;
	ARN?: string;
	Comment?: string;
	Status?: string;
	LastModifiedTime?: string;
};

export function cloudfrontHref(s: {
	account: string;
	distribution?: string;
	tab?: CloudFrontTab;
}) {
	const params = new URLSearchParams({ account: s.account });
	if (s.distribution) params.set("distribution", s.distribution);
	else if (s.tab && s.tab !== "distributions") params.set("tab", s.tab);
	return `/services/cloudfront?${params}`;
}

export function cloudfrontCall<T = Record<string, unknown>>(
	s: CloudFrontScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("cloudfront", operation, s.account, input);
}

// pages follows NextMarker until the service stops returning one.
async function pages<T>(
	s: CloudFrontScope,
	operation: string,
	list: string,
	input: Record<string, unknown> = {},
): Promise<T[]> {
	const items: T[] = [],
		seen = new Set<string>();
	let marker: string | undefined;
	do {
		const out = await cloudfrontCall<
			Record<string, { Items?: T[]; NextMarker?: string } | undefined>
		>(s, operation, {
			...input,
			MaxItems: 100,
			...(marker ? { Marker: marker } : {}),
		});
		items.push(...(out[list]?.Items ?? []));
		marker = out[list]?.NextMarker;
		if (marker && seen.has(marker))
			throw new Error("The server returned a repeated page marker.");
		if (marker) seen.add(marker);
	} while (marker);
	return items;
}

export const listDistributions = (s: CloudFrontScope) =>
	pages<DistributionSummary>(s, "ListDistributions", "DistributionList");
export const listOacs = (s: CloudFrontScope) =>
	pages<OacSummary>(s, "ListOriginAccessControls", "OriginAccessControlList");
export const listKeyValueStores = (s: CloudFrontScope) =>
	pages<KeyValueStore>(s, "ListKeyValueStores", "KeyValueStoreList");
export const listInvalidations = (s: CloudFrontScope, distribution: string) =>
	pages<InvalidationSummary>(s, "ListInvalidations", "InvalidationList", {
		DistributionId: distribution,
	});

export const getDistribution = (s: CloudFrontScope, id: string) =>
	cloudfrontCall<{ Distribution: Distribution; ETag: string }>(
		s,
		"GetDistribution",
		{ Id: id },
	);

export async function listTags(s: CloudFrontScope, arn: string) {
	return (
		(
			await cloudfrontCall<{ Tags?: TagList }>(s, "ListTagsForResource", {
				Resource: arn,
			})
		).Tags ?? {}
	);
}

// Distributions, access controls and key value stores change only with the ETag of
// their latest version. The helpers below read it right before they write, so a
// change is never refused because the page showed an older version.

export async function updateDistribution(
	s: CloudFrontScope,
	id: string,
	change: (config: DistributionConfig) => DistributionConfig,
) {
	const { DistributionConfig, ETag } = await cloudfrontCall<{
		DistributionConfig: DistributionConfig;
		ETag: string;
	}>(s, "GetDistributionConfig", { Id: id });
	return cloudfrontCall(s, "UpdateDistribution", {
		Id: id,
		IfMatch: ETag,
		DistributionConfig: change(DistributionConfig),
	});
}

export async function deleteDistribution(s: CloudFrontScope, id: string) {
	const { ETag } = await cloudfrontCall<{ ETag: string }>(
		s,
		"GetDistributionConfig",
		{ Id: id },
	);
	return cloudfrontCall(s, "DeleteDistribution", { Id: id, IfMatch: ETag });
}

export async function updateOac(
	s: CloudFrontScope,
	id: string,
	config: OacConfig,
) {
	const { ETag } = await cloudfrontCall<{ ETag: string }>(
		s,
		"GetOriginAccessControlConfig",
		{ Id: id },
	);
	return cloudfrontCall(s, "UpdateOriginAccessControl", {
		Id: id,
		IfMatch: ETag,
		OriginAccessControlConfig: config,
	});
}

export async function deleteOac(s: CloudFrontScope, id: string) {
	const { ETag } = await cloudfrontCall<{ ETag: string }>(
		s,
		"GetOriginAccessControlConfig",
		{ Id: id },
	);
	return cloudfrontCall(s, "DeleteOriginAccessControl", {
		Id: id,
		IfMatch: ETag,
	});
}

export function oacForm(oac: OacSummary): OacForm {
	return {
		name: oac.Name ?? "",
		description: oac.Description ?? "",
		originType: oac.OriginAccessControlOriginType ?? "s3",
		signingBehavior: oac.SigningBehavior ?? "always",
		signingProtocol: oac.SigningProtocol ?? "sigv4",
	};
}

export async function updateKeyValueStore(
	s: CloudFrontScope,
	name: string,
	comment: string,
) {
	const { ETag } = await cloudfrontCall<{ ETag: string }>(
		s,
		"DescribeKeyValueStore",
		{ Name: name },
	);
	return cloudfrontCall(s, "UpdateKeyValueStore", {
		Name: name,
		IfMatch: ETag,
		Comment: comment,
	});
}

export async function deleteKeyValueStore(s: CloudFrontScope, name: string) {
	const { ETag } = await cloudfrontCall<{ ETag: string }>(
		s,
		"DescribeKeyValueStore",
		{ Name: name },
	);
	return cloudfrontCall(s, "DeleteKeyValueStore", {
		Name: name,
		IfMatch: ETag,
	});
}
