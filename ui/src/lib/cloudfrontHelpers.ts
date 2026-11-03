// Pure logic for the CloudFront page: payload builders, checks and display summaries.
import { isValidDomain } from "./acmHelpers.ts";
import { lines, parseInteger, tagsFromRows } from "./elbv2Helpers.ts";
import { tagList } from "./tags.ts";

// CloudFront lists carry their length in Quantity beside Items; an empty list has no Items.
export type Quantified<T> = { Quantity?: number; Items?: T[] };
export type Origin = {
	Id?: string;
	DomainName?: string;
	OriginPath?: string;
	OriginAccessControlId?: string;
	S3OriginConfig?: { OriginAccessIdentity?: string; [member: string]: unknown };
	CustomOriginConfig?: {
		HTTPPort?: number;
		HTTPSPort?: number;
		OriginProtocolPolicy?: string;
		[member: string]: unknown;
	};
	[member: string]: unknown;
};
export type Behavior = {
	PathPattern?: string;
	TargetOriginId?: string;
	ViewerProtocolPolicy?: string;
	AllowedMethods?: Quantified<string> & { CachedMethods?: Quantified<string> };
	Compress?: boolean;
	CachePolicyId?: string;
	[member: string]: unknown;
};
export type DistributionConfig = {
	CallerReference?: string;
	Comment?: string;
	Enabled?: boolean;
	Aliases?: Quantified<string>;
	DefaultRootObject?: string;
	Origins?: Quantified<Origin>;
	OriginGroups?: Quantified<{ Id?: string }>;
	DefaultCacheBehavior?: Behavior;
	CacheBehaviors?: Quantified<Behavior>;
	[member: string]: unknown;
};

export const viewerProtocolPolicies = [
	"allow-all",
	"redirect-to-https",
	"https-only",
];
export const originProtocolPolicies = [
	"https-only",
	"http-only",
	"match-viewer",
];
export const methodChoices = [
	{ value: "read", label: "GET, HEAD", methods: ["GET", "HEAD"] },
	{
		value: "options",
		label: "GET, HEAD, OPTIONS",
		methods: ["GET", "HEAD", "OPTIONS"],
	},
	{
		value: "all",
		label: "GET, HEAD, OPTIONS, PUT, POST, PATCH, DELETE",
		methods: ["GET", "HEAD", "OPTIONS", "PUT", "POST", "PATCH", "DELETE"],
	},
];
// CloudFront's managed cache policies; TinyStack does not implement custom ones.
export const managedCachePolicies = [
	{
		id: "658327ea-f89d-4fab-a63d-7e88639e58f6",
		name: "Managed-CachingOptimized",
	},
	{
		id: "4135ea2d-6df8-44a3-9df3-4b5a84be39ad",
		name: "Managed-CachingDisabled",
	},
];
export const oacOriginTypes = ["s3", "mediastore", "lambda", "mediapackagev2"];
export const oacSigningBehaviors = ["always", "never", "no-override"];
export const oacSigningProtocols = ["sigv4"];

export function quantified<T>(items: T[]): Quantified<T> {
	return { Quantity: items.length, ...(items.length ? { Items: items } : {}) };
}

function without<T extends object>(value: T, members: string[]): Partial<T> {
	return Object.fromEntries(
		Object.entries(value).filter(([member]) => !members.includes(member)),
	) as Partial<T>;
}

// Origins ---------------------------------------------------------------

export type OriginKind = "s3" | "custom" | "other";
export type OriginForm = {
	// was is the ID the origin had when the form was loaded; a new origin has none.
	was: string;
	id: string;
	kind: OriginKind;
	domain: string;
	path: string;
	accessControl: string;
	protocol: string;
	httpPort: string;
	httpsPort: string;
};

export function originKind(origin: Origin): OriginKind {
	if (origin.S3OriginConfig) return "s3";
	return origin.CustomOriginConfig ? "custom" : "other";
}

export function emptyOrigin(kind: OriginKind = "s3"): OriginForm {
	return {
		was: "",
		id: "",
		kind,
		domain: "",
		path: "",
		accessControl: "",
		protocol: "https-only",
		httpPort: "80",
		httpsPort: "443",
	};
}

// originId is the ID an origin gets: the one typed, or else its domain name.
export const originId = (o: OriginForm) => o.id.trim() || o.domain.trim();

function originForm(origin: Origin): OriginForm {
	const custom = origin.CustomOriginConfig;
	return {
		...emptyOrigin(originKind(origin)),
		was: origin.Id ?? "",
		id: origin.Id ?? "",
		domain: origin.DomainName ?? "",
		path: origin.OriginPath ?? "",
		accessControl: origin.OriginAccessControlId ?? "",
		protocol: custom?.OriginProtocolPolicy ?? "https-only",
		httpPort: String(custom?.HTTPPort ?? 80),
		httpsPort: String(custom?.HTTPSPort ?? 443),
	};
}

const hostPattern = /^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$/;

// originItem builds one origin from its form, keeping what the form does not edit
// from previous, the origin with the same ID in the configuration being updated.
function originItem(form: OriginForm, previous?: Origin): Origin {
	const id = originId(form);
	if (form.kind === "other") {
		if (!previous)
			throw new Error(
				`Origin ${id}: this type of origin cannot be added here.`,
			);
		return previous;
	}
	const domain = form.domain.trim();
	if (!domain || domain.length > 253 || !hostPattern.test(domain))
		throw new Error(
			`Origin ${id || "(new)"}: enter a domain name without a scheme, path or port.`,
		);
	const path = form.path.trim();
	if (path && (!path.startsWith("/") || path.endsWith("/")))
		throw new Error(
			`Origin ${id}: the path must start with a slash and not end with one.`,
		);
	const kept = without(previous ?? {}, [
		"S3OriginConfig",
		"CustomOriginConfig",
		"OriginAccessControlId",
		"OriginPath",
	]);
	const common = {
		...kept,
		Id: id,
		DomainName: domain,
		...(path ? { OriginPath: path } : {}),
	};
	if (form.kind === "s3") {
		const identity = previous?.S3OriginConfig?.OriginAccessIdentity ?? "";
		return {
			...common,
			S3OriginConfig: {
				...previous?.S3OriginConfig,
				// An origin access control and an origin access identity exclude each other.
				OriginAccessIdentity: form.accessControl ? "" : identity,
			},
			...(form.accessControl
				? { OriginAccessControlId: form.accessControl }
				: {}),
		};
	}
	if (!originProtocolPolicies.includes(form.protocol))
		throw new Error(`Origin ${id}: choose a protocol policy.`);
	return {
		...common,
		CustomOriginConfig: {
			...previous?.CustomOriginConfig,
			HTTPPort: parseInteger(form.httpPort, `Origin ${id} HTTP port`, 1, 65535),
			HTTPSPort: parseInteger(
				form.httpsPort,
				`Origin ${id} HTTPS port`,
				1,
				65535,
			),
			OriginProtocolPolicy: form.protocol,
		},
	};
}

// Distributions ---------------------------------------------------------

export type DistributionForm = {
	comment: string;
	enabled: boolean;
	aliases: string;
	defaultRootObject: string;
	origins: OriginForm[];
	target: string;
	viewerProtocol: string;
	methods: string;
	compress: boolean;
	cachePolicyId: string;
	tags: { key: string; value: string }[];
};

export function emptyDistributionForm(): DistributionForm {
	return {
		comment: "",
		enabled: true,
		aliases: "",
		defaultRootObject: "",
		origins: [emptyOrigin()],
		target: "",
		viewerProtocol: "redirect-to-https",
		methods: "read",
		compress: true,
		cachePolicyId: managedCachePolicies[0].id,
		tags: [],
	};
}

export function methodsKey(methods: string[] = []) {
	return (
		methodChoices.find(
			(c) =>
				c.methods.length === methods.length &&
				c.methods.every((m) => methods.includes(m)),
		)?.value ?? "read"
	);
}

// distributionForm fills the form with what a configuration holds, for editing it.
export function distributionForm(config: DistributionConfig): DistributionForm {
	const behavior = config.DefaultCacheBehavior ?? {};
	return {
		comment: config.Comment ?? "",
		enabled: config.Enabled ?? false,
		aliases: (config.Aliases?.Items ?? []).join("\n"),
		defaultRootObject: config.DefaultRootObject ?? "",
		origins: (config.Origins?.Items ?? []).map(originForm),
		target: behavior.TargetOriginId ?? "",
		viewerProtocol: behavior.ViewerProtocolPolicy ?? "allow-all",
		methods: methodsKey(behavior.AllowedMethods?.Items),
		compress: behavior.Compress ?? false,
		cachePolicyId: behavior.CachePolicyId ?? "",
		tags: [],
	};
}

// defaultTarget is the origin the default behavior sends requests to: the one chosen,
// or else the only origin.
export function defaultTarget(form: DistributionForm) {
	const ids = form.origins.map(originId).filter(Boolean);
	return form.target || (ids.length === 1 ? ids[0] : "");
}

export function parseAliases(text: string) {
	const aliases = lines(text);
	for (const alias of aliases)
		if (!isValidDomain(alias))
			throw new Error(`${alias} is not a valid alternate domain name.`);
	if (new Set(aliases.map((a) => a.toLowerCase())).size !== aliases.length)
		throw new Error("Alternate domain names must be unique.");
	return aliases;
}

// distributionConfig applies the form to base, the configuration being updated
// (just a caller reference when creating). Members the form does not edit stay as
// they are, so updating cannot drop cache behaviors, certificates or error pages.
export function distributionConfig(
	base: DistributionConfig,
	form: DistributionForm,
): DistributionConfig {
	const comment = form.comment.trim();
	if (comment.length > 128)
		throw new Error("The comment can be at most 128 characters.");
	const aliases = parseAliases(form.aliases);
	const root = form.defaultRootObject.trim();
	if (root.startsWith("/"))
		throw new Error("The default root object must not start with a slash.");
	if (!form.origins.length) throw new Error("Add at least one origin.");
	const previous = new Map(
		(base.Origins?.Items ?? []).map((o) => [o.Id ?? "", o]),
	);
	const origins = form.origins.map((o) =>
		originItem(o, o.was ? previous.get(o.was) : undefined),
	);
	const ids = origins.map((o) => o.Id ?? "");
	const duplicate = ids.find((id, i) => ids.indexOf(id) !== i);
	if (duplicate) throw new Error(`Origin ID ${duplicate} is used twice.`);
	// A behavior may also send requests to an origin group, which this form leaves alone.
	const targets = new Set(
		[...ids, ...(base.OriginGroups?.Items ?? []).map((g) => g.Id ?? "")].filter(
			Boolean,
		),
	);
	const target = defaultTarget(form);
	if (!targets.has(target))
		throw new Error(
			"Choose the origin the default behavior sends requests to.",
		);
	for (const b of base.CacheBehaviors?.Items ?? [])
		if (!targets.has(b.TargetOriginId ?? ""))
			throw new Error(
				`The cache behavior ${b.PathPattern ?? ""} sends requests to origin ${b.TargetOriginId}, which is not an origin of this distribution.`,
			);
	if (!viewerProtocolPolicies.includes(form.viewerProtocol))
		throw new Error("Choose a viewer protocol policy.");
	const choice = methodChoices.find((c) => c.value === form.methods);
	if (!choice) throw new Error("Choose the allowed HTTP methods.");
	const policy = form.cachePolicyId.trim();
	if (/\s/.test(policy))
		throw new Error("The cache policy ID must not contain spaces.");
	const behavior = base.DefaultCacheBehavior ?? {};
	// Cached methods stay when they are still allowed, as CloudFront permits GET and HEAD
	// with or without OPTIONS.
	const cached = behavior.AllowedMethods?.CachedMethods;
	const keepCached =
		!!cached?.Items?.length &&
		cached.Items.every((m) => choice.methods.includes(m));
	return {
		...base,
		Comment: comment,
		Enabled: form.enabled,
		Aliases: quantified(aliases),
		DefaultRootObject: root,
		Origins: quantified(origins),
		DefaultCacheBehavior: {
			...without(behavior, ["CachePolicyId"]),
			TargetOriginId: target,
			ViewerProtocolPolicy: form.viewerProtocol,
			AllowedMethods: {
				...quantified(choice.methods),
				CachedMethods: keepCached ? cached : quantified(["GET", "HEAD"]),
			},
			Compress: form.compress,
			...(policy ? { CachePolicyId: policy } : {}),
		},
	};
}

export type TagItems = { Items: { Key: string; Value: string }[] };

export function tagItems(tags: Record<string, string>): TagItems {
	return {
		Items: tagList(tags),
	};
}

export type TagList = { Items?: { Key?: string; Value?: string }[] };

export function tagMap(tags?: TagList) {
	return Object.fromEntries(
		(tags?.Items ?? []).map((t) => [t.Key ?? "", t.Value ?? ""]),
	);
}

export function callerReference(
	prefix: string,
	now = Date.now(),
	nonce = Math.random().toString(36).slice(2, 10),
) {
	return `${prefix}-${now}-${nonce}`;
}

export function createDistributionInput(
	form: DistributionForm,
	reference: string,
) {
	return {
		DistributionConfigWithTags: {
			DistributionConfig: distributionConfig(
				{ CallerReference: reference },
				form,
			),
			Tags: tagItems(tagsFromRows(form.tags)),
		},
	};
}

// Display summaries -------------------------------------------------------

export function originsText(config?: DistributionConfig) {
	return (config?.Origins?.Items ?? []).map((o) => o.DomainName).join(", ");
}

export function originRows(config?: DistributionConfig) {
	const kinds = { s3: "S3", custom: "Custom", other: "Other" };
	return (config?.Origins?.Items ?? []).map((o) => ({
		id: o.Id ?? "-",
		domain: o.DomainName ?? "-",
		kind: kinds[originKind(o)],
		path: o.OriginPath || "-",
		accessControl: o.OriginAccessControlId || "-",
	}));
}

export function cachePolicyName(id?: string) {
	return managedCachePolicies.find((p) => p.id === id)?.name ?? id ?? "-";
}

export function behaviorRows(config?: DistributionConfig) {
	const row = (b: Behavior, path: string) => ({
		path,
		target: b.TargetOriginId ?? "-",
		viewerProtocol: b.ViewerProtocolPolicy ?? "-",
		methods: (b.AllowedMethods?.Items ?? []).join(", ") || "-",
		compress: b.Compress ? "Yes" : "No",
		cachePolicy: cachePolicyName(b.CachePolicyId),
	});
	return [
		...(config?.DefaultCacheBehavior
			? [row(config.DefaultCacheBehavior, "Default (*)")]
			: []),
		...(config?.CacheBehaviors?.Items ?? []).map((b) =>
			row(b, b.PathPattern ?? "-"),
		),
	];
}

export const distributionStatusType = (status?: string) =>
	status === "Deployed" ? "success" : "in-progress";

// Invalidations ---------------------------------------------------------

export const maxInvalidationPaths = 3000;

export function invalidationPaths(text: string) {
	const paths = [
		...new Set(
			text
				.split("\n")
				.map((p) => p.trim())
				.filter(Boolean),
		),
	];
	if (!paths.length) throw new Error("Enter at least one path, one per line.");
	if (paths.length > maxInvalidationPaths)
		throw new Error(
			`An invalidation can have at most ${maxInvalidationPaths} paths.`,
		);
	for (const path of paths) {
		if (!path.startsWith("/"))
			throw new Error(`${path} must start with a slash.`);
		if (path.includes("*") && path.indexOf("*") !== path.length - 1)
			throw new Error(`${path}: an asterisk can only end a path.`);
	}
	return paths;
}

export function createInvalidationInput(
	distributionId: string,
	text: string,
	reference: string,
) {
	return {
		DistributionId: distributionId,
		InvalidationBatch: {
			CallerReference: reference,
			Paths: quantified(invalidationPaths(text)),
		},
	};
}

// Origin access controls --------------------------------------------------

export type OacForm = {
	name: string;
	description: string;
	originType: string;
	signingBehavior: string;
	signingProtocol: string;
};

export const emptyOacForm: OacForm = {
	name: "",
	description: "",
	originType: "s3",
	signingBehavior: "always",
	signingProtocol: "sigv4",
};

export function oacConfig(form: OacForm) {
	const name = form.name.trim();
	if (!name || name.length > 64)
		throw new Error("The name must be 1 to 64 characters.");
	const description = form.description.trim();
	if (description.length > 256)
		throw new Error("The description can be at most 256 characters.");
	for (const [value, choices, label] of [
		[form.originType, oacOriginTypes, "an origin type"],
		[form.signingBehavior, oacSigningBehaviors, "a signing behavior"],
		[form.signingProtocol, oacSigningProtocols, "a signing protocol"],
	] as const)
		if (!choices.includes(value)) throw new Error(`Choose ${label}.`);
	return {
		Name: name,
		...(description ? { Description: description } : {}),
		OriginAccessControlOriginType: form.originType,
		SigningBehavior: form.signingBehavior,
		SigningProtocol: form.signingProtocol,
	};
}

export type OacConfig = ReturnType<typeof oacConfig>;

// Key value stores --------------------------------------------------------

const kvsNamePattern = /^[A-Za-z0-9_-]{1,64}$/;

export function kvsName(name: string) {
	const value = name.trim();
	if (!kvsNamePattern.test(value))
		throw new Error(
			"The name must be 1 to 64 letters, digits, hyphens or underscores.",
		);
	return value;
}

export function kvsComment(comment: string) {
	const value = comment.trim();
	if (value.length > 128)
		throw new Error("The comment can be at most 128 characters.");
	return value;
}

export function createKvsInput(form: {
	name: string;
	comment: string;
	tags: { key: string; value: string }[];
}) {
	const tags = tagsFromRows(form.tags);
	return {
		Name: kvsName(form.name),
		Comment: kvsComment(form.comment),
		...(Object.keys(tags).length ? { Tags: tagItems(tags) } : {}),
	};
}

export function kvsStatusType(status?: string) {
	if (status === "READY") return "success";
	return status === "PROVISIONING" ? "in-progress" : "error";
}
