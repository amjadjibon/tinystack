import { api } from "./api";
import {
	creationDate,
	type DomainConfig,
	type DomainRow,
	type DomainStatus,
} from "./opensearchHelpers";
import { type Tag, tagMap } from "./tags";

export type OpenSearchScope = { account: string; region: string };

export function opensearchHref(s: {
	account: string;
	region: string;
	domain?: string;
}) {
	const params = new URLSearchParams({ account: s.account, region: s.region });
	if (s.domain) params.set("domain", s.domain);
	return `/services/opensearch?${params}`;
}

export function opensearchCall<T = Record<string, unknown>>(
	s: OpenSearchScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("opensearch", operation, s.account, input, s.region);
}

// DescribeDomains takes at most five names.
const describeBatch = 5;

export async function getDomain(
	s: OpenSearchScope,
	name: string,
): Promise<DomainStatus> {
	const out = await opensearchCall<{ DomainStatus?: DomainStatus }>(
		s,
		"DescribeDomain",
		{ DomainName: name },
	);
	if (!out.DomainStatus) throw new Error(`Domain ${name} was not found.`);
	return out.DomainStatus;
}

export async function getDomainConfig(
	s: OpenSearchScope,
	name: string,
): Promise<DomainConfig> {
	const out = await opensearchCall<{ DomainConfig?: DomainConfig }>(
		s,
		"DescribeDomainConfig",
		{ DomainName: name },
	);
	return out.DomainConfig ?? {};
}

// getDomainDetail reads a domain with the state of each of its configuration options.
export async function getDomainDetail(s: OpenSearchScope, name: string) {
	const [domain, config] = await Promise.all([
		getDomain(s, name),
		getDomainConfig(s, name),
	]);
	return { domain, config };
}

// listDomains describes every domain in the Region. A domain's creation time is on its configuration, not on its
// status, and a domain deleted since the names were listed has none.
export async function listDomains(s: OpenSearchScope): Promise<DomainRow[]> {
	const listed = await opensearchCall<{
		DomainNames?: { DomainName?: string }[];
	}>(s, "ListDomainNames");
	const names = (listed.DomainNames ?? []).flatMap((d) =>
		d.DomainName ? [d.DomainName] : [],
	);
	const batches: string[][] = [];
	for (let i = 0; i < names.length; i += describeBatch)
		batches.push(names.slice(i, i + describeBatch));
	const described = await Promise.all(
		batches.map((batch) =>
			opensearchCall<{ DomainStatusList?: DomainStatus[] }>(
				s,
				"DescribeDomains",
				{ DomainNames: batch },
			),
		),
	);
	return Promise.all(
		described
			.flatMap((out) => out.DomainStatusList ?? [])
			.map(async (domain) => ({
				...domain,
				CreationDate: await getDomainConfig(s, domain.DomainName).then(
					creationDate,
					() => undefined,
				),
			})),
	);
}

export async function listTags(s: OpenSearchScope, arn: string) {
	const out = await opensearchCall<{ TagList?: Tag[] }>(s, "ListTags", {
		ARN: arn,
	});
	return tagMap(out.TagList);
}
