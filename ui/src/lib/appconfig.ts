import { api } from "./api";
import {
	type Application,
	type ConfigurationProfile,
	type Deployment,
	type DeploymentStrategy,
	type DeploymentSummary,
	type Environment,
	type FetchedConfiguration,
	type HostedVersion,
	newestDeploymentFirst,
	newestVersionFirst,
	type ProfileSummary,
	sortByName,
	sortStrategies,
	type VersionSummary,
} from "./appconfigHelpers";

export type AppConfigScope = { account: string; region: string };
// The first two are the tabs of the page; the others are the tabs of an application.
export type AppConfigTab =
	| "applications"
	| "strategies"
	| "environments"
	| "profiles"
	| "fetch";
export const appconfigTabs: AppConfigTab[] = [
	"applications",
	"strategies",
	"environments",
	"profiles",
	"fetch",
];

export function appconfigHref(s: {
	account: string;
	region: string;
	application?: string;
	environment?: string;
	profile?: string;
	tab?: AppConfigTab;
}) {
	const params = new URLSearchParams({ account: s.account, region: s.region });
	if (s.application) {
		params.set("application", s.application);
		if (s.environment) params.set("environment", s.environment);
		else if (s.profile) params.set("profile", s.profile);
		else if (s.tab && s.tab !== "environments") params.set("tab", s.tab);
	} else if (s.tab && s.tab !== "applications") params.set("tab", s.tab);
	return `/services/appconfig?${params}`;
}

export function appconfigCall<T = Record<string, unknown>>(
	s: AppConfigScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("appconfig", operation, s.account, input, s.region);
}

// The data plane is a service of its own, with its own console operations.
function dataCall<T>(
	s: AppConfigScope,
	operation: string,
	input: Record<string, unknown>,
): Promise<T> {
	return api.action("appconfigdata", operation, s.account, input, s.region);
}

// listAll collects every page of a list operation, which AppConfig pages with Items and NextToken.
async function listAll<T>(
	s: AppConfigScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T[]> {
	const items: T[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const out = await appconfigCall<{ Items?: T[]; NextToken?: string }>(
			s,
			operation,
			{ ...input, MaxResults: 50, ...(token ? { NextToken: token } : {}) },
		);
		items.push(...(out.Items ?? []));
		token = out.NextToken;
		if (token && seen.has(token))
			throw new Error("The server returned a repeated page token.");
		if (token) seen.add(token);
	} while (token);
	return items;
}

export const listApplications = async (s: AppConfigScope) =>
	sortByName(await listAll<Application>(s, "ListApplications"));

export const getApplication = (s: AppConfigScope, id: string) =>
	appconfigCall<Application>(s, "GetApplication", { ApplicationId: id });

export const listEnvironments = async (
	s: AppConfigScope,
	application: string,
) =>
	sortByName(
		await listAll<Environment>(s, "ListEnvironments", {
			ApplicationId: application,
		}),
	);

export const getEnvironment = (
	s: AppConfigScope,
	application: string,
	id: string,
) =>
	appconfigCall<Environment>(s, "GetEnvironment", {
		ApplicationId: application,
		EnvironmentId: id,
	});

export const listProfiles = async (s: AppConfigScope, application: string) =>
	sortByName(
		await listAll<ProfileSummary>(s, "ListConfigurationProfiles", {
			ApplicationId: application,
		}),
	);

export const getProfile = (
	s: AppConfigScope,
	application: string,
	id: string,
) =>
	appconfigCall<ConfigurationProfile>(s, "GetConfigurationProfile", {
		ApplicationId: application,
		ConfigurationProfileId: id,
	});

export const listStrategies = async (s: AppConfigScope) =>
	sortStrategies(
		await listAll<DeploymentStrategy>(s, "ListDeploymentStrategies"),
	);

export const listVersions = async (
	s: AppConfigScope,
	application: string,
	profile: string,
) =>
	newestVersionFirst(
		await listAll<VersionSummary>(s, "ListHostedConfigurationVersions", {
			ApplicationId: application,
			ConfigurationProfileId: profile,
		}),
	);

export const getVersion = (
	s: AppConfigScope,
	application: string,
	profile: string,
	version: number,
) =>
	appconfigCall<HostedVersion>(s, "GetHostedConfigurationVersion", {
		ApplicationId: application,
		ConfigurationProfileId: profile,
		VersionNumber: version,
	});

export const listDeployments = async (
	s: AppConfigScope,
	application: string,
	environment: string,
) =>
	newestDeploymentFirst(
		await listAll<DeploymentSummary>(s, "ListDeployments", {
			ApplicationId: application,
			EnvironmentId: environment,
		}),
	);

export const getDeployment = (
	s: AppConfigScope,
	application: string,
	environment: string,
	number: number,
) =>
	appconfigCall<Deployment>(s, "GetDeployment", {
		ApplicationId: application,
		EnvironmentId: environment,
		DeploymentNumber: number,
	});

// Applications carry no tags in their description, so tags are read apart, by ARN.
export async function listTags(s: AppConfigScope, arn: string) {
	const out = await appconfigCall<{ Tags?: Record<string, string> }>(
		s,
		"ListTagsForResource",
		{ ResourceArn: arn },
	);
	return out.Tags ?? {};
}

// fetchConfiguration reads what is deployed to an environment as a client of the data
// plane would. A new session's first poll returns the latest deployed content.
export async function fetchConfiguration(
	s: AppConfigScope,
	ids: { application: string; environment: string; profile: string },
): Promise<FetchedConfiguration> {
	const session = await dataCall<{ InitialConfigurationToken: string }>(
		s,
		"StartConfigurationSession",
		{
			ApplicationIdentifier: ids.application,
			EnvironmentIdentifier: ids.environment,
			ConfigurationProfileIdentifier: ids.profile,
		},
	);
	return dataCall<FetchedConfiguration>(s, "GetLatestConfiguration", {
		ConfigurationToken: session.InitialConfigurationToken,
	});
}
