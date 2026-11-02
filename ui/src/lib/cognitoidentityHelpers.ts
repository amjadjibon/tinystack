// Pure logic for the Cognito identity pools page. It mirrors what
// internal/services/cognitoidentity accepts so forms reject bad input before a request is sent.
import { jsonObject, stringMap } from "./consoleActions.ts";

export type UserPoolProvider = {
	ProviderName: string;
	ClientId?: string;
	ServerSideTokenCheck?: boolean;
};
export type IdentityPool = {
	IdentityPoolId: string;
	IdentityPoolName: string;
	AllowUnauthenticatedIdentities?: boolean;
	AllowClassicFlow?: boolean;
	SupportedLoginProviders?: Record<string, string>;
	DeveloperProviderName?: string;
	OpenIdConnectProviderARNs?: string[];
	CognitoIdentityProviders?: UserPoolProvider[];
	IdentityPoolTags?: Record<string, string>;
};
export type MappingRule = {
	Claim: string;
	MatchType: string;
	Value: string;
	RoleARN: string;
};
export type RoleMapping = {
	Type: string;
	AmbiguousRoleResolution: string;
	RulesConfiguration?: { Rules: MappingRule[] };
};
export type PoolRoles = {
	Roles?: Record<string, string>;
	RoleMappings?: Record<string, RoleMapping>;
};
export type Identity = {
	IdentityId: string;
	Logins?: string[];
	CreationDate?: number;
	LastModifiedDate?: number;
};
export type Credentials = {
	AccessKeyId?: string;
	SecretKey?: string;
	SessionToken?: string;
	Expiration?: number;
};

// Login providers a pool may name in SupportedLoginProviders. TinyStack keeps
// their app IDs but does not verify their tokens.
export const socialProviders = [
	"graph.facebook.com",
	"accounts.google.com",
	"www.amazon.com",
	"api.twitter.com",
	"www.digits.com",
	"appleid.apple.com",
];
export const mappingTypes = ["Token", "Rules"];
export const resolutions = ["AuthenticatedRole", "Deny"];
export const matchTypes = ["Equals", "NotEqual", "StartsWith", "Contains"];
export const maxRules = 25;
// ListIdentityPools, ListIdentities and DeleteIdentities take 1 to 60 items per call.
export const callLimit = 60;

const namePattern = /^[\w\s+=,.@-]{1,128}$/;
const developerPattern = /^[\w.-]{1,128}$/;
const userPoolPattern =
	/^cognito-idp\.([a-z0-9-]+)\.amazonaws\.com\/([a-z0-9-]+_[A-Za-z0-9]+)$/;

export function poolArn(account: string, region: string, id: string) {
	return `arn:aws:cognito-identity:${region}:${account}:identitypool/${id}`;
}

export function userPoolProvider(region: string, userPoolId: string) {
	return `cognito-idp.${region}.amazonaws.com/${userPoolId}`;
}

// userPoolRef is the Region and ID of the user pool a provider name points at.
export function userPoolRef(provider: string) {
	const m = userPoolPattern.exec(provider);
	return m ? { region: m[1], id: m[2] } : undefined;
}

export function isRoleArn(arn: string) {
	return arn.startsWith("arn:aws:iam::") && arn.includes(":role/");
}

export function roleName(arn: string) {
	return arn.split(":role/").at(-1) || arn;
}

export type ProviderEntry = { kind: string; name: string; detail: string };

// providerEntries lists everything that can authenticate into the pool.
export function providerEntries(p: IdentityPool): ProviderEntry[] {
	return [
		...(p.CognitoIdentityProviders ?? []).map((u) => ({
			kind: "Cognito user pool",
			name: u.ProviderName,
			detail:
				[
					u.ClientId ? `App client ${u.ClientId}` : "Any app client",
					u.ServerSideTokenCheck ? "server-side token check" : "",
				]
					.filter(Boolean)
					.join(", ") || "-",
		})),
		...(p.OpenIdConnectProviderARNs ?? []).map((arn) => ({
			kind: "OpenID Connect",
			name: arn,
			detail: "-",
		})),
		...Object.entries(p.SupportedLoginProviders ?? {}).map(([name, appId]) => ({
			kind: "Social login",
			name,
			detail: `App ID ${appId}`,
		})),
		...(p.DeveloperProviderName
			? [{ kind: "Developer", name: p.DeveloperProviderName, detail: "-" }]
			: []),
	];
}

// providerSummary is the kinds of provider a pool has, with counts above one.
export function providerSummary(p: IdentityPool) {
	const counts = new Map<string, number>();
	for (const { kind } of providerEntries(p))
		counts.set(kind, (counts.get(kind) ?? 0) + 1);
	return (
		[...counts]
			.map(([kind, n]) => (n > 1 ? `${kind} (${n})` : kind))
			.join(", ") || "-"
	);
}

export type ProviderRow = {
	id: string;
	name: string;
	clientId: string;
	serverSideCheck: boolean;
};
export type PoolForm = {
	name: string;
	allowUnauthenticated: boolean;
	allowClassic: boolean;
	developerProvider: string;
	userPools: ProviderRow[];
	social: Record<string, string>;
	oidc: string;
	tags: string;
};

let rows = 0;
// emptyProviderRow gives each row an ID so React can tell rows apart as they are removed.
export function emptyProviderRow(): ProviderRow {
	rows += 1;
	return {
		id: `provider-${rows}`,
		name: "",
		clientId: "",
		serverSideCheck: false,
	};
}

export function emptyPoolForm(): PoolForm {
	return {
		name: "",
		allowUnauthenticated: false,
		allowClassic: false,
		developerProvider: "",
		userPools: [],
		social: {},
		oidc: "",
		tags: "{}",
	};
}

export function formFromPool(p: IdentityPool): PoolForm {
	return {
		name: p.IdentityPoolName,
		allowUnauthenticated: !!p.AllowUnauthenticatedIdentities,
		allowClassic: !!p.AllowClassicFlow,
		developerProvider: p.DeveloperProviderName ?? "",
		userPools: (p.CognitoIdentityProviders ?? []).map((u) => ({
			...emptyProviderRow(),
			name: u.ProviderName,
			clientId: u.ClientId ?? "",
			serverSideCheck: !!u.ServerSideTokenCheck,
		})),
		social: { ...p.SupportedLoginProviders },
		oidc: (p.OpenIdConnectProviderARNs ?? []).join("\n"),
		tags: "{}",
	};
}

// poolInput builds CreateIdentityPool's input, or with an id UpdateIdentityPool's.
// An update replaces every setting, so anything left blank is cleared; tags are
// kept as they are and edited on their own.
export function poolInput(f: PoolForm, id?: string): Record<string, unknown> {
	const name = f.name.trim();
	if (!namePattern.test(name))
		throw new Error(
			"Name must be 1 to 128 letters, digits, spaces or the characters + = , . @ - _",
		);
	const developer = f.developerProvider.trim();
	if (developer && !developerPattern.test(developer))
		throw new Error(
			"The developer provider name must be 1 to 128 letters, digits, periods, hyphens or underscores.",
		);
	const userPools = f.userPools.map((row) => {
		const provider = row.name.trim();
		if (!userPoolPattern.test(provider))
			throw new Error(
				`${provider || "A user pool provider"} must look like cognito-idp.us-east-1.amazonaws.com/us-east-1_AbCdEfGhI.`,
			);
		const clientId = row.clientId.trim();
		return {
			ProviderName: provider,
			...(clientId ? { ClientId: clientId } : {}),
			...(row.serverSideCheck ? { ServerSideTokenCheck: true } : {}),
		};
	});
	const social: Record<string, string> = {};
	for (const [provider, appId] of Object.entries(f.social)) {
		if (!socialProviders.includes(provider))
			throw new Error(`${provider} is not a supported login provider.`);
		if (appId.trim()) social[provider] = appId.trim();
	}
	const oidc = [
		...new Set(
			f.oidc
				.split("\n")
				.map((arn) => arn.trim())
				.filter(Boolean),
		),
	];
	for (const arn of oidc)
		if (!arn.startsWith("arn:aws:iam::") || !arn.includes(":oidc-provider/"))
			throw new Error(
				`${arn} must be an IAM OpenID Connect provider ARN such as arn:aws:iam::000000000000:oidc-provider/example.com.`,
			);
	const tags = id || !f.tags.trim() ? {} : stringMap(f.tags, "Tags");
	return {
		...(id ? { IdentityPoolId: id } : {}),
		IdentityPoolName: name,
		AllowUnauthenticatedIdentities: f.allowUnauthenticated,
		AllowClassicFlow: f.allowClassic,
		...(developer ? { DeveloperProviderName: developer } : {}),
		...(userPools.length ? { CognitoIdentityProviders: userPools } : {}),
		...(Object.keys(social).length ? { SupportedLoginProviders: social } : {}),
		...(oidc.length ? { OpenIdConnectProviderARNs: oidc } : {}),
		...(Object.keys(tags).length ? { IdentityPoolTags: tags } : {}),
	};
}

export type RolesForm = {
	authenticated: string;
	unauthenticated: string;
	mappings: string;
};

export function formFromRoles(r: PoolRoles): RolesForm {
	return {
		authenticated: r.Roles?.authenticated ?? "",
		unauthenticated: r.Roles?.unauthenticated ?? "",
		mappings: JSON.stringify(r.RoleMappings ?? {}, null, 2),
	};
}

// mappingKeys are the keys a role mapping can use for the pool's providers: a
// user pool by itself or with one app client, and an OpenID Connect provider by
// its host and path.
export function mappingKeys(p: IdentityPool): string[] {
	return [
		...(p.CognitoIdentityProviders ?? []).map((u) =>
			u.ClientId ? `${u.ProviderName}:${u.ClientId}` : u.ProviderName,
		),
		...(p.OpenIdConnectProviderARNs ?? []).map(
			(arn) => arn.split(":oidc-provider/").at(-1) ?? arn,
		),
	];
}

function record(value: unknown, label: string): Record<string, unknown> {
	if (!value || typeof value !== "object" || Array.isArray(value))
		throw new Error(`${label} must be a JSON object.`);
	return value as Record<string, unknown>;
}

function members(
	value: Record<string, unknown>,
	allowed: string[],
	label: string,
) {
	for (const key of Object.keys(value))
		if (!allowed.includes(key))
			throw new Error(`${label}.${key} is not supported.`);
}

function required(value: unknown, label: string): string {
	if (typeof value !== "string" || !value)
		throw new Error(`${label} is required.`);
	return value;
}

function oneOf(value: unknown, allowed: string[], label: string): string {
	if (typeof value !== "string" || !allowed.includes(value))
		throw new Error(`${label} must be ${allowed.join(" or ")}.`);
	return value;
}

function roleArn(value: unknown, label: string): string {
	const arn = required(value, label);
	if (!isRoleArn(arn))
		throw new Error(
			`${label} must be an IAM role ARN such as arn:aws:iam::000000000000:role/name.`,
		);
	return arn;
}

function mappingRule(value: unknown, label: string): MappingRule {
	const rule = record(value, label);
	members(rule, ["Claim", "MatchType", "Value", "RoleARN"], label);
	return {
		Claim: required(rule.Claim, `${label}.Claim`),
		MatchType: oneOf(rule.MatchType, matchTypes, `${label}.MatchType`),
		Value: required(rule.Value, `${label}.Value`),
		RoleARN: roleArn(rule.RoleARN, `${label}.RoleARN`),
	};
}

// parseMappings checks role mappings, a JSON object of mappings by provider key.
export function parseMappings(text: string): Record<string, RoleMapping> {
	const input = jsonObject(text.trim() || "{}", "Role mappings");
	const out: Record<string, RoleMapping> = {};
	for (const [key, value] of Object.entries(input)) {
		if (!key.trim() || key.length > 128)
			throw new Error("Role mapping keys must be 1 to 128 characters.");
		const label = `Role mapping ${key}`;
		const mapping = record(value, label);
		members(
			mapping,
			["Type", "AmbiguousRoleResolution", "RulesConfiguration"],
			label,
		);
		const type = oneOf(mapping.Type, mappingTypes, `${label}.Type`);
		const parsed: RoleMapping = {
			Type: type,
			AmbiguousRoleResolution: oneOf(
				mapping.AmbiguousRoleResolution,
				resolutions,
				`${label}.AmbiguousRoleResolution`,
			),
		};
		if (type === "Token") {
			if (mapping.RulesConfiguration !== undefined)
				throw new Error(
					`${label}.RulesConfiguration applies only to Rules mappings.`,
				);
		} else {
			if (mapping.RulesConfiguration === undefined)
				throw new Error(
					`${label}.RulesConfiguration is required for Rules mappings.`,
				);
			const config = record(
				mapping.RulesConfiguration,
				`${label}.RulesConfiguration`,
			);
			members(config, ["Rules"], `${label}.RulesConfiguration`);
			const rules = config.Rules;
			if (!Array.isArray(rules) || rules.length < 1 || rules.length > maxRules)
				throw new Error(
					`${label}.RulesConfiguration.Rules must list 1 to ${maxRules} rules.`,
				);
			parsed.RulesConfiguration = {
				Rules: rules.map((rule, i) =>
					mappingRule(rule, `${label}.Rules[${i}]`),
				),
			};
		}
		out[key] = parsed;
	}
	return out;
}

// rolesInput builds SetIdentityPoolRoles' input, which replaces the pool's roles and mappings.
export function rolesInput(
	poolId: string,
	f: RolesForm,
): Record<string, unknown> {
	const roles: Record<string, string> = {};
	for (const kind of ["authenticated", "unauthenticated"] as const) {
		const arn = f[kind].trim();
		if (arn) roles[kind] = roleArn(arn, `The ${kind} role`);
	}
	const mappings = parseMappings(f.mappings);
	return {
		IdentityPoolId: poolId,
		Roles: roles,
		...(Object.keys(mappings).length ? { RoleMappings: mappings } : {}),
	};
}

export function ruleText(r: MappingRule) {
	return `${r.Claim} ${r.MatchType} "${r.Value}" → ${roleName(r.RoleARN)}`;
}

// identityKind says how an identity authenticates: with no logins it is unauthenticated.
export function identityKind(i: Identity) {
	return i.Logins?.length ? "Authenticated" : "Unauthenticated";
}

export function chunks<T>(items: T[], size: number): T[][] {
	const out: T[][] = [];
	for (let i = 0; i < items.length; i += size)
		out.push(items.slice(i, i + size));
	return out;
}
