// Pure logic for the AppConfig page: payload builders, checks and display summaries.
import { parseInteger, tagsFromRows } from "./elbv2Helpers.ts";

export type TagRow = { key: string; value: string };
export type Application = { Id: string; Name?: string; Description?: string };
export type Environment = {
	ApplicationId?: string;
	Id: string;
	Name?: string;
	Description?: string;
	State?: string;
};
export type Validator = { Type?: string; Content?: string };
export type ConfigurationProfile = {
	ApplicationId?: string;
	Id: string;
	Name?: string;
	Description?: string;
	LocationUri?: string;
	Type?: string;
	Validators?: Validator[];
};
// A profile as the list reports it, without its description and validators.
export type ProfileSummary = {
	ApplicationId?: string;
	Id: string;
	Name?: string;
	LocationUri?: string;
	Type?: string;
	ValidatorTypes?: string[];
};
export type DeploymentStrategy = {
	Id: string;
	Name?: string;
	Description?: string;
	DeploymentDurationInMinutes?: number;
	FinalBakeTimeInMinutes?: number;
	GrowthFactor?: number;
	GrowthType?: string;
	ReplicateTo?: string;
};
export type VersionSummary = {
	ApplicationId?: string;
	ConfigurationProfileId?: string;
	VersionNumber: number;
	Description?: string;
	ContentType?: string;
	VersionLabel?: string;
};
// Console actions carry blobs as base64, so Content is base64 text.
export type HostedVersion = VersionSummary & { Content?: string };
export type DeploymentSummary = {
	DeploymentNumber?: number;
	ConfigurationProfileId?: string;
	ConfigurationName?: string;
	ConfigurationVersion?: string;
	VersionLabel?: string;
	DeploymentDurationInMinutes?: number;
	FinalBakeTimeInMinutes?: number;
	GrowthFactor?: number;
	GrowthType?: string;
	State?: string;
	PercentageComplete?: number;
	StartedAt?: string;
	CompletedAt?: string;
};
export type Deployment = DeploymentSummary & {
	ApplicationId?: string;
	EnvironmentId?: string;
	DeploymentStrategyId?: string;
	ConfigurationLocationUri?: string;
	Description?: string;
};
// What the data plane returns for one poll; Configuration is base64.
export type FetchedConfiguration = {
	Configuration?: string;
	ContentType?: string;
	VersionLabel?: string;
	NextPollIntervalInSeconds?: number;
};

export const profileTypes = ["AWS.Freeform", "AWS.AppConfig.FeatureFlags"];
export const validatorTypes = ["JSON_SCHEMA", "LAMBDA"];
export const growthTypes = ["LINEAR", "EXPONENTIAL"];
const contentTypes = ["application/json", "application/x-yaml", "text/plain"];
export const maxValidators = 2;
const maxContentBytes = 1024 * 1024;
const maxNameLength = 64;
const maxDescriptionLength = 1024;
const maxLabelLength = 64;
const maxSchemaBytes = 32768;
const maxMinutes = 1440;
const predefinedPrefix = "AppConfig.";

const characters = (text: string) => [...text].length;

// Names and descriptions ----------------------------------------------------

export function checkName(name: string, label = "Name") {
	const value = name.trim();
	if (!value) throw new Error(`${label} is required.`);
	if (characters(value) > maxNameLength)
		throw new Error(`${label} must have at most ${maxNameLength} characters.`);
	return value;
}

export function checkDescription(text: string) {
	const value = text.trim();
	if (characters(value) > maxDescriptionLength)
		throw new Error(
			`Description must have at most ${maxDescriptionLength} characters.`,
		);
	return value;
}

export type NamedForm = { name: string; description: string };

export function formFromNamed(item: {
	Name?: string;
	Description?: string;
}): NamedForm {
	return { name: item.Name ?? "", description: item.Description ?? "" };
}

// nameChanges lists only the name and description an edit changes. A description
// that was emptied is sent as an empty string, which clears it.
function nameChanges(
	current: { Name?: string; Description?: string },
	form: NamedForm,
) {
	const name = checkName(form.name);
	const description = checkDescription(form.description);
	return {
		...(name !== (current.Name ?? "") ? { Name: name } : {}),
		...(description !== (current.Description ?? "")
			? { Description: description }
			: {}),
	};
}

function required<T extends object>(changes: T) {
	if (!Object.keys(changes).length)
		throw new Error("Change at least one of the fields.");
	return changes;
}

// Applications -------------------------------------------------------------

export type ApplicationForm = NamedForm & { tags: TagRow[] };

export const emptyApplicationForm = (): ApplicationForm => ({
	name: "",
	description: "",
	tags: [],
});

export function createApplicationInput(form: ApplicationForm) {
	const description = checkDescription(form.description);
	const tags = tagsFromRows(form.tags);
	return {
		Name: checkName(form.name),
		...(description ? { Description: description } : {}),
		...(Object.keys(tags).length ? { Tags: tags } : {}),
	};
}

export function updateApplicationInput(app: Application, form: NamedForm) {
	return {
		ApplicationId: app.Id,
		...required(nameChanges(app, form)),
	};
}

export function applicationArn(
	scope: { account: string; region: string },
	id: string,
) {
	return `arn:aws:appconfig:${scope.region}:${scope.account}:application/${id}`;
}

// Environments -------------------------------------------------------------

export function createEnvironmentInput(applicationId: string, form: NamedForm) {
	const description = checkDescription(form.description);
	return {
		ApplicationId: applicationId,
		Name: checkName(form.name),
		...(description ? { Description: description } : {}),
	};
}

export function updateEnvironmentInput(
	applicationId: string,
	env: Environment,
	form: NamedForm,
) {
	return {
		ApplicationId: applicationId,
		EnvironmentId: env.Id,
		...required(nameChanges(env, form)),
	};
}

// Configuration profiles ---------------------------------------------------

export type ValidatorRow = { type: string; content: string };
export type ProfileForm = NamedForm & {
	type: string;
	validators: ValidatorRow[];
};

export const emptyProfileForm = (): ProfileForm => ({
	name: "",
	description: "",
	type: profileTypes[0],
	validators: [],
});

export function formFromProfile(p: ConfigurationProfile): ProfileForm {
	return {
		...formFromNamed(p),
		type: p.Type ?? profileTypes[0],
		validators: (p.Validators ?? []).map((v) => ({
			type: v.Type ?? "",
			content: v.Content ?? "",
		})),
	};
}

const lambdaArn =
	/^arn:aws[a-z-]*:lambda:[a-z0-9-]+:\d{12}:function:[\w-]+(:[\w$-]+)?$/;

// validatorInputs checks the validator rows and returns the members that carry them.
// A JSON Schema is a JSON object and a Lambda validator is the function's ARN.
export function validatorInputs(rows: ValidatorRow[]): Validator[] {
	if (rows.length > maxValidators)
		throw new Error(`A profile can have at most ${maxValidators} validators.`);
	return rows.map((row, i) => {
		const label = `Validator ${i + 1}`;
		const content = row.content.trim();
		if (!validatorTypes.includes(row.type))
			throw new Error(`${label}: choose ${validatorTypes.join(" or ")}.`);
		if (row.type === "LAMBDA") {
			if (!lambdaArn.test(content))
				throw new Error(`${label}: enter the ARN of a Lambda function.`);
		} else {
			if (new TextEncoder().encode(content).length > maxSchemaBytes)
				throw new Error(
					`${label}: the schema must have at most ${maxSchemaBytes} bytes.`,
				);
			let schema: unknown;
			try {
				schema = JSON.parse(content);
			} catch {
				throw new Error(`${label}: the schema must be valid JSON.`);
			}
			if (!schema || typeof schema !== "object" || Array.isArray(schema))
				throw new Error(`${label}: the schema must be a JSON object.`);
		}
		return { Type: row.type, Content: content };
	});
}

export function createProfileInput(applicationId: string, form: ProfileForm) {
	if (!profileTypes.includes(form.type))
		throw new Error("Choose a configuration type.");
	const description = checkDescription(form.description);
	const validators = validatorInputs(form.validators);
	return {
		ApplicationId: applicationId,
		Name: checkName(form.name),
		LocationUri: "hosted",
		Type: form.type,
		...(description ? { Description: description } : {}),
		...(validators.length ? { Validators: validators } : {}),
	};
}

const sameValidators = (a: Validator[], b: Validator[]) =>
	a.length === b.length &&
	a.every((v, i) => v.Type === b[i].Type && v.Content === b[i].Content);

// updateProfileInput changes the name, description and validators. The type and
// location are fixed when the profile is created.
export function updateProfileInput(
	applicationId: string,
	profile: ConfigurationProfile,
	form: ProfileForm,
) {
	const validators = validatorInputs(form.validators);
	return {
		ApplicationId: applicationId,
		ConfigurationProfileId: profile.Id,
		...required({
			...nameChanges(profile, form),
			...(sameValidators(validators, profile.Validators ?? [])
				? {}
				: { Validators: validators }),
		}),
	};
}

// prettyContent re-indents JSON and leaves any other text as it is.
export function prettyContent(text: string) {
	try {
		return JSON.stringify(JSON.parse(text), null, 2);
	} catch {
		return text;
	}
}

// Deployment strategies ----------------------------------------------------

export type StrategyForm = NamedForm & {
	duration: string;
	bake: string;
	factor: string;
	growth: string;
};

export const emptyStrategyForm = (): StrategyForm => ({
	name: "",
	description: "",
	duration: "10",
	bake: "10",
	factor: "20",
	growth: "LINEAR",
});

export function formFromStrategy(s: DeploymentStrategy): StrategyForm {
	return {
		...formFromNamed(s),
		duration: String(s.DeploymentDurationInMinutes ?? 0),
		bake: String(s.FinalBakeTimeInMinutes ?? 0),
		factor: String(s.GrowthFactor ?? ""),
		growth: s.GrowthType ?? "LINEAR",
	};
}

// strategyFields checks the rollout settings both create and edit carry.
function strategyFields(form: StrategyForm) {
	const factor = form.factor.trim();
	const amount = Number(factor);
	if (!/^\d+(\.\d+)?$/.test(factor) || amount < 1 || amount > 100)
		throw new Error("Growth factor must be a number from 1 to 100.");
	if (!growthTypes.includes(form.growth))
		throw new Error(`Choose ${growthTypes.join(" or ")} growth.`);
	return {
		DeploymentDurationInMinutes: parseInteger(
			form.duration,
			"Deployment time",
			0,
			maxMinutes,
		),
		FinalBakeTimeInMinutes: parseInteger(form.bake, "Bake time", 0, maxMinutes),
		GrowthFactor: amount,
		GrowthType: form.growth,
	};
}

// createStrategyInput builds CreateDeploymentStrategy. The service requires
// ReplicateTo and replicates nowhere.
export function createStrategyInput(form: StrategyForm) {
	const name = checkName(form.name);
	if (name.startsWith(predefinedPrefix))
		throw new Error(
			`Names that start with ${predefinedPrefix} belong to the AWS strategies.`,
		);
	const description = checkDescription(form.description);
	return {
		Name: name,
		...(description ? { Description: description } : {}),
		...strategyFields(form),
		ReplicateTo: "NONE",
	};
}

// updateStrategyInput builds UpdateDeploymentStrategy, which cannot rename a strategy.
export function updateStrategyInput(
	strategy: DeploymentStrategy,
	form: StrategyForm,
) {
	const fields = strategyFields(form);
	const description = checkDescription(form.description);
	const changes = {
		...(description !== (strategy.Description ?? "")
			? { Description: description }
			: {}),
		...(fields.DeploymentDurationInMinutes !==
		strategy.DeploymentDurationInMinutes
			? { DeploymentDurationInMinutes: fields.DeploymentDurationInMinutes }
			: {}),
		...(fields.FinalBakeTimeInMinutes !== strategy.FinalBakeTimeInMinutes
			? { FinalBakeTimeInMinutes: fields.FinalBakeTimeInMinutes }
			: {}),
		...(fields.GrowthFactor !== strategy.GrowthFactor
			? { GrowthFactor: fields.GrowthFactor }
			: {}),
		...(fields.GrowthType !== strategy.GrowthType
			? { GrowthType: fields.GrowthType }
			: {}),
	};
	return { DeploymentStrategyId: strategy.Id, ...required(changes) };
}

// The four strategies AWS defines cannot be changed or deleted.
export const isPredefined = (s: Pick<DeploymentStrategy, "Id">) =>
	s.Id.startsWith(predefinedPrefix);

// sortStrategies puts the AWS strategies first, then the custom ones by name.
export function sortStrategies(items: DeploymentStrategy[]) {
	return [...items].sort(
		(a, b) =>
			Number(isPredefined(b)) - Number(isPredefined(a)) ||
			(a.Name ?? "").localeCompare(b.Name ?? ""),
	);
}

export const sortByName = <T extends { Name?: string }>(items: T[]) =>
	[...items].sort((a, b) => (a.Name ?? "").localeCompare(b.Name ?? ""));

const plain = (n?: number) => (n === undefined ? "-" : String(n));

export function minutesText(minutes?: number) {
	return minutes === undefined ? "-" : `${minutes} min`;
}

// growthText reads "LINEAR 20%" for the way a rollout grows.
export function growthText(s: { GrowthType?: string; GrowthFactor?: number }) {
	return s.GrowthType === undefined
		? "-"
		: `${s.GrowthType} ${plain(s.GrowthFactor)}%`;
}

// rolloutText summarizes a strategy's settings, as in "LINEAR 50% over 1 min, bake 1 min".
export function rolloutText(s: {
	GrowthType?: string;
	GrowthFactor?: number;
	DeploymentDurationInMinutes?: number;
	FinalBakeTimeInMinutes?: number;
}) {
	return `${growthText(s)} over ${minutesText(s.DeploymentDurationInMinutes)}, bake ${minutesText(s.FinalBakeTimeInMinutes)}`;
}

export function strategyLabel(s: DeploymentStrategy) {
	return isPredefined(s) ? s.Id : `${s.Name ?? s.Id} (${s.Id})`;
}

// Hosted configuration versions --------------------------------------------

export type VersionForm = {
	contentType: string;
	content: string;
	description: string;
	label: string;
};

export const emptyVersionForm = (): VersionForm => ({
	contentType: contentTypes[0],
	content: "",
	description: "",
	label: "",
});

// toBase64 encodes bytes the way console actions carry blobs.
export function toBase64(bytes: Uint8Array) {
	let binary = "";
	for (let i = 0; i < bytes.length; i += 0x8000)
		binary += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
	return btoa(binary);
}

// decodeContent reads a version's base64 content as text when it is UTF-8. Binary
// content has no text, only a size.
export function decodeContent(base64?: string): {
	text?: string;
	size: number;
} {
	const bytes = Uint8Array.from(atob(base64 ?? ""), (c) => c.charCodeAt(0));
	try {
		return {
			text: new TextDecoder("utf-8", { fatal: true }).decode(bytes),
			size: bytes.length,
		};
	} catch {
		return { size: bytes.length };
	}
}

// formFromVersion starts a new version from an existing text version.
export function formFromVersion(
	v: Pick<VersionSummary, "ContentType">,
	text: string,
): VersionForm {
	return {
		...emptyVersionForm(),
		contentType: v.ContentType ?? "",
		content: text,
	};
}

// contentTypeChoices lists the content types the editor offers, and the one a copied
// version already has when it is not among them.
export function contentTypeChoices(current: string) {
	return !current || contentTypes.includes(current)
		? contentTypes
		: [...contentTypes, current];
}

function checkContentType(text: string) {
	const value = text.trim();
	if (value.length > 255 || !/^[\w!#$&^.+-]+\/[\w!#$&^.+-]+$/.test(value))
		throw new Error("Content type must look like application/json.");
	return value;
}

// checkLabel lets a label name a version without being mistaken for a version number.
export function checkLabel(text: string) {
	const label = text.trim();
	if (!label) return "";
	if (characters(label) > maxLabelLength)
		throw new Error(
			`Version label must have at most ${maxLabelLength} characters.`,
		);
	if (!/[^0-9]/.test(label))
		throw new Error(
			"Version label must have a character that is not a digit, or it reads as a version number.",
		);
	return label;
}

export function createVersionInput(
	applicationId: string,
	profileId: string,
	form: VersionForm,
) {
	const contentType = checkContentType(form.contentType);
	if (!form.content.trim()) throw new Error("Enter the configuration content.");
	const bytes = new TextEncoder().encode(form.content);
	if (bytes.length > maxContentBytes)
		throw new Error("Content must have at most 1 MiB.");
	if (contentType === "application/json") {
		try {
			JSON.parse(form.content);
		} catch (err) {
			throw new Error(
				`Content is not valid JSON${err instanceof Error ? `: ${err.message}` : "."}`,
			);
		}
	}
	const description = checkDescription(form.description);
	const label = checkLabel(form.label);
	return {
		ApplicationId: applicationId,
		ConfigurationProfileId: profileId,
		Content: toBase64(bytes),
		ContentType: contentType,
		...(description ? { Description: description } : {}),
		...(label ? { VersionLabel: label } : {}),
	};
}

export const newestVersionFirst = (items: VersionSummary[]) =>
	[...items].sort((a, b) => b.VersionNumber - a.VersionNumber);

// versionText reads "3 (stable)" for a version number and its label.
export function versionText(version?: number | string, label?: string) {
	if (version === undefined || version === "") return "-";
	return label ? `${version} (${label})` : String(version);
}

export function bytesText(size: number) {
	return size === 1 ? "1 byte" : `${size} bytes`;
}

// Deployments --------------------------------------------------------------

export type DeploymentForm = {
	profile: string;
	version: string;
	strategy: string;
	description: string;
};

export const emptyDeploymentForm = (): DeploymentForm => ({
	profile: "",
	version: "",
	strategy: "",
	description: "",
});

// startDeploymentInput builds StartDeployment; version is the version number.
export function startDeploymentInput(
	applicationId: string,
	environmentId: string,
	form: DeploymentForm,
) {
	if (!form.profile) throw new Error("Choose a configuration profile.");
	if (!form.version) throw new Error("Choose a hosted configuration version.");
	if (!form.strategy) throw new Error("Choose a deployment strategy.");
	const description = checkDescription(form.description);
	return {
		ApplicationId: applicationId,
		EnvironmentId: environmentId,
		ConfigurationProfileId: form.profile,
		ConfigurationVersion: form.version,
		DeploymentStrategyId: form.strategy,
		...(description ? { Description: description } : {}),
	};
}

const inProgress = ["BAKING", "VALIDATING", "DEPLOYING", "ROLLING_BACK"];
export const deploymentInProgress = (state?: string) =>
	!!state && inProgress.includes(state);
export const hasActiveDeployments = (items: { State?: string }[]) =>
	items.some((d) => deploymentInProgress(d.State));

// stateStatusType maps a deployment or environment state to a status indicator.
export function stateStatusType(state?: string) {
	switch (state) {
		case "COMPLETE":
		case "READY_FOR_DEPLOYMENT":
			return "success" as const;
		case "BAKING":
		case "VALIDATING":
		case "DEPLOYING":
		case "ROLLING_BACK":
			return "in-progress" as const;
		case "ROLLED_BACK":
			return "error" as const;
		case "REVERTED":
			return "stopped" as const;
		default:
			return "pending" as const;
	}
}

export function percentText(percent?: number) {
	return percent === undefined ? "-" : `${Math.round(percent * 10) / 10}%`;
}

export const newestDeploymentFirst = <T extends { DeploymentNumber?: number }>(
	items: T[],
) =>
	[...items].sort(
		(a, b) => (b.DeploymentNumber ?? 0) - (a.DeploymentNumber ?? 0),
	);

// canStop reports whether StopDeployment applies. The service reverts only the
// latest completed deployment.
export function canStop(
	d: DeploymentSummary,
	deployments: DeploymentSummary[],
) {
	const latest = Math.max(
		0,
		...deployments.map((x) => x.DeploymentNumber ?? 0),
	);
	return d.State === "COMPLETE" && d.DeploymentNumber === latest;
}

// stopDeploymentInput builds StopDeployment. A completed deployment is stopped by
// reverting it, so the service requires AllowRevert.
export function stopDeploymentInput(
	applicationId: string,
	environmentId: string,
	deployment: DeploymentSummary,
) {
	return {
		ApplicationId: applicationId,
		EnvironmentId: environmentId,
		DeploymentNumber: deployment.DeploymentNumber,
		AllowRevert: true,
	};
}

// Links from the resource browser --------------------------------------------

// inventoryTarget finds the application, environment or profile an inventory record
// belongs to from its ARN: application/<id>[/environment/<id>[/deployment/<n>] |
// /configurationprofile/<id>[/hostedconfigurationversion/<n>]] or deploymentstrategy/<id>.
export function inventoryTarget(arn: string) {
	const path = arn.split(":").slice(5).join(":").split("/");
	const after = (kind: string) => {
		const at = path.indexOf(kind);
		return at < 0 ? undefined : path[at + 1];
	};
	if (path[0] === "deploymentstrategy") return { tab: "strategies" as const };
	const environment = after("environment");
	return {
		application: after("application"),
		environment,
		profile: environment ? undefined : after("configurationprofile"),
	};
}
