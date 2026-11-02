import { api } from "./api";
import type {
	EventDestination,
	SendQuota,
	Tag,
	TemplateContent,
} from "./sesv2Helpers";

export type SESScope = { account: string; region: string };
export type IdentityInfo = {
	IdentityType?: string;
	IdentityName?: string;
	SendingEnabled?: boolean;
	VerificationStatus?: string;
};
export type Dkim = {
	SigningEnabled?: boolean;
	Status?: string;
	Tokens?: string[];
	SigningAttributesOrigin?: string;
	NextSigningKeyLength?: string;
	CurrentSigningKeyLength?: string;
	LastKeyGenerationTimestamp?: number;
};
export type IdentityDetails = {
	IdentityType?: string;
	FeedbackForwardingStatus?: boolean;
	VerifiedForSendingStatus?: boolean;
	VerificationStatus?: string;
	ConfigurationSetName?: string;
	DkimAttributes?: Dkim;
	Tags?: Tag[];
};
export type ConfigSetDetails = {
	ConfigurationSetName?: string;
	DeliveryOptions?: {
		TlsPolicy?: string;
		SendingPoolName?: string;
		MaxDeliverySeconds?: number;
	};
	ReputationOptions?: { ReputationMetricsEnabled?: boolean };
	SendingOptions?: { SendingEnabled?: boolean };
	SuppressionOptions?: { SuppressedReasons?: string[] };
	TrackingOptions?: { CustomRedirectDomain?: string; HttpsPolicy?: string };
	Tags?: Tag[];
};
export type TemplateMetadata = {
	TemplateName?: string;
	CreatedTimestamp?: number;
};
export type TemplateDetails = {
	TemplateName?: string;
	TemplateContent?: TemplateContent;
	Tags?: Tag[];
};
export type AccountDetails = {
	SendingEnabled?: boolean;
	ProductionAccessEnabled?: boolean;
	DedicatedIpAutoWarmupEnabled?: boolean;
	EnforcementStatus?: string;
	SendQuota?: SendQuota;
};

export function sesv2Href(s: SESScope) {
	return `/services/sesv2?${new URLSearchParams({ account: s.account, region: s.region })}`;
}

export function sesv2Call<T = Record<string, unknown>>(
	s: SESScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("sesv2", operation, s.account, input, s.region);
}

async function pages<T>(
	s: SESScope,
	operation: string,
	key: string,
	size: number,
): Promise<T[]> {
	const items: T[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const out = await sesv2Call<Record<string, unknown>>(s, operation, {
			PageSize: size,
			...(token ? { NextToken: token } : {}),
		});
		items.push(...((out[key] as T[] | undefined) ?? []));
		token = out.NextToken as string | undefined;
		if (token && seen.has(token))
			throw new Error("The server returned a repeated page token.");
		if (token) seen.add(token);
	} while (token);
	return items;
}

export const listIdentities = (s: SESScope) =>
	pages<IdentityInfo>(s, "ListEmailIdentities", "EmailIdentities", 100);
export const listConfigSets = (s: SESScope) =>
	pages<string>(s, "ListConfigurationSets", "ConfigurationSets", 100);
export const listTemplates = (s: SESScope) =>
	pages<TemplateMetadata>(s, "ListEmailTemplates", "TemplatesMetadata", 100);

export const getAccount = (s: SESScope) =>
	sesv2Call<AccountDetails>(s, "GetAccount");
export const getIdentity = (s: SESScope, name: string) =>
	sesv2Call<IdentityDetails>(s, "GetEmailIdentity", { EmailIdentity: name });
export const getConfigSet = (s: SESScope, name: string) =>
	sesv2Call<ConfigSetDetails>(s, "GetConfigurationSet", {
		ConfigurationSetName: name,
	});
export const getDestinations = async (s: SESScope, name: string) =>
	(
		await sesv2Call<{ EventDestinations?: EventDestination[] }>(
			s,
			"GetConfigurationSetEventDestinations",
			{ ConfigurationSetName: name },
		)
	).EventDestinations ?? [];
export const getTemplate = (s: SESScope, name: string) =>
	sesv2Call<TemplateDetails>(s, "GetEmailTemplate", { TemplateName: name });

export function resourceArn(
	s: SESScope,
	kind: "identity" | "configuration-set" | "template",
	name: string,
) {
	return `arn:aws:ses:${s.region}:${s.account}:${kind}/${name}`;
}

export async function listTags(s: SESScope, arn: string) {
	return (
		(
			await sesv2Call<{ Tags?: Tag[] }>(s, "ListTagsForResource", {
				ResourceArn: arn,
			})
		).Tags ?? []
	);
}
