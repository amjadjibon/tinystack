import { allKeyTypes, type DomainValidation } from "./acmHelpers";
import { api } from "./api";

export type ACMScope = {
	account: string;
	region: string;
	certificate?: string;
};
export type CertificateSummary = {
	CertificateArn: string;
	DomainName: string;
	SubjectAlternativeNameSummaries?: string[];
	HasAdditionalSubjectAlternativeNames?: boolean;
	Status: string;
	Type: string;
	KeyAlgorithm?: string;
	InUse?: boolean;
	NotAfter?: number;
};
export type CertificateDetail = {
	CertificateArn: string;
	DomainName: string;
	SubjectAlternativeNames?: string[];
	DomainValidationOptions?: DomainValidation[];
	Serial?: string;
	Subject?: string;
	Issuer?: string;
	CreatedAt?: number;
	IssuedAt?: number;
	ImportedAt?: number;
	Status: string;
	NotBefore?: number;
	NotAfter?: number;
	KeyAlgorithm?: string;
	SignatureAlgorithm?: string;
	InUseBy?: string[];
	Type: string;
	KeyUsages?: { Name: string }[];
	ExtendedKeyUsages?: { Name: string; OID?: string }[];
	RenewalEligibility?: string;
	Options?: {
		CertificateTransparencyLoggingPreference?: string;
		Export?: string;
	};
};
export function acmHref(s: ACMScope) {
	const query = new URLSearchParams({ account: s.account, region: s.region });
	if (s.certificate) query.set("certificate", s.certificate);
	return `/services/acm?${query}`;
}
export function acmCall<T = Record<string, unknown>>(
	s: ACMScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("acm", operation, s.account, input, s.region);
}
export async function listCertificates(
	s: ACMScope,
	statuses: string[],
): Promise<CertificateSummary[]> {
	const items: CertificateSummary[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const out = await acmCall<{
			CertificateSummaryList?: CertificateSummary[];
			NextToken?: string;
		}>(s, "ListCertificates", {
			Includes: { keyTypes: allKeyTypes },
			MaxItems: 100,
			...(statuses.length ? { CertificateStatuses: statuses } : {}),
			...(token ? { NextToken: token } : {}),
		});
		items.push(...(out.CertificateSummaryList ?? []));
		token = out.NextToken;
		if (token && seen.has(token))
			throw new Error("The server returned a repeated page token.");
		if (token) seen.add(token);
	} while (token);
	return items;
}
