import { Alert, Box, SpaceBetween, Tabs } from "@cloudscape-design/components";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useEffect } from "react";
import {
	ApplicationDetails,
	type ApplicationTab,
} from "#/components/appconfig/ApplicationDetails";
import { Applications } from "#/components/appconfig/Applications";
import { EnvironmentDetails } from "#/components/appconfig/EnvironmentDetails";
import { ProfileDetails } from "#/components/appconfig/ProfileDetails";
import { Strategies } from "#/components/appconfig/Strategies";
import { ConsoleLayout } from "#/components/Console";
import {
	type AppConfigScope,
	type AppConfigTab,
	appconfigHref,
	appconfigTabs,
	getApplication,
	getEnvironment,
	getProfile,
} from "#/lib/appconfig";
import type {
	Application,
	ConfigurationProfile,
	Environment,
} from "#/lib/appconfigHelpers";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { type Poll, usePoll } from "#/lib/usePoll";

export const Route = createFileRoute("/services/appconfig")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
		application: String(search.application ?? ""),
		environment: String(search.environment ?? ""),
		profile: String(search.profile ?? ""),
		tab: appconfigTabs.find((t) => t === search.tab),
	}),
	component: AppConfigPage,
});

const applicationTabs: ApplicationTab[] = ["environments", "profiles", "fetch"];

// useRecord loads the record the address names, and loads again when the address names another.
function useRecord<T extends { Id: string }>(
	id: string,
	enabled: boolean,
	load: (id: string) => Promise<T>,
): Poll<T> {
	const poll = usePoll<T | undefined>(
		() => (id && enabled ? load(id) : Promise.resolve(undefined)),
		0,
	);
	const { reload } = poll;
	useEffect(() => {
		if (id && enabled) reload();
	}, [id, enabled, reload]);
	return { ...poll, data: poll.data?.Id === id ? poll.data : undefined };
}

function AppConfigPage() {
	const search = Route.useSearch();
	const { canWrite, account, region, capabilities, inventory } = useScope();
	const enabled =
		!!capabilities.data?.pages.appconfig &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const scope = { account, region };
	const home = appconfigHref(scope);
	const strategies = appconfigHref({ ...scope, tab: "strategies" });
	const inApplication = search.application !== "";
	const application = useRecord(search.application, enabled, (id) =>
		getApplication(scope, id),
	);
	const environment = useRecord(
		inApplication ? search.environment : "",
		enabled,
		(id) => getEnvironment(scope, search.application, id),
	);
	const profile = useRecord(
		inApplication && !search.environment ? search.profile : "",
		enabled,
		(id) => getProfile(scope, search.application, id),
	);
	const applicationHref = appconfigHref({
		...scope,
		application: search.application,
	});
	return (
		<ConsoleLayout
			contentType={inApplication ? "default" : "table"}
			navigationHeader={{ text: "AppConfig", href: home }}
			activeHref={
				!inApplication && search.tab === "strategies" ? strategies : home
			}
			navigationItems={[
				{ type: "link", text: "Applications", href: home },
				{ type: "link", text: "Deployment strategies", href: strategies },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "AppConfig", href: home },
				...(!inApplication && search.tab === "strategies"
					? [{ text: "Deployment strategies", href: strategies }]
					: []),
				...(inApplication
					? [
							{
								text: application.data?.Name ?? search.application,
								href: applicationHref,
							},
						]
					: []),
				...(inApplication && search.environment
					? [
							{
								text: environment.data?.Name ?? search.environment,
								href: appconfigHref({
									...scope,
									application: search.application,
									environment: search.environment,
								}),
							},
						]
					: []),
				...(inApplication && !search.environment && search.profile
					? [
							{
								text: profile.data?.Name ?? search.profile,
								href: appconfigHref({
									...scope,
									application: search.application,
									profile: search.profile,
								}),
							},
						]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{!!(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.appconfig && (
						<Alert type="warning">
							AppConfig is not enabled on this server.
						</Alert>
					)}
					{!canWrite && (
						<Alert type="info">
							Connect a console session for this account to manage AppConfig.
						</Alert>
					)}
					<Workspace
						key={`${account}/${region}/${enabled}`}
						scope={scope}
						enabled={enabled}
						application={application}
						environment={environment}
						profile={profile}
						tab={search.tab}
						selected={{
							application: search.application,
							environment: search.environment,
							profile: search.profile,
						}}
						onChanged={inventory.reload}
					/>
				</SpaceBetween>
			}
		/>
	);
}

// Pending stands in for a record that has not loaded, or cannot.
function Pending({
	error,
	enabled,
	what,
}: {
	error?: string;
	enabled: boolean;
	what: string;
}) {
	if (error) return <Alert type="error">{error}</Alert>;
	if (!enabled)
		return <Alert type="info">Connect a console session to load {what}.</Alert>;
	return <Box>Loading {what}…</Box>;
}

function Workspace({
	scope,
	enabled,
	application,
	environment,
	profile,
	tab,
	selected,
	onChanged,
}: {
	scope: AppConfigScope;
	enabled: boolean;
	application: Poll<Application>;
	environment: Poll<Environment>;
	profile: Poll<ConfigurationProfile>;
	tab?: AppConfigTab;
	selected: { application: string; environment: string; profile: string };
	onChanged: () => void;
}) {
	const navigate = useNavigate();
	const go = (href: string) => navigate({ href });
	if (!selected.application)
		return (
			<Tabs
				activeTabId={tab === "strategies" ? "strategies" : "applications"}
				onChange={({ detail }) =>
					go(
						appconfigHref({
							...scope,
							tab: detail.activeTabId as AppConfigTab,
						}),
					)
				}
				tabs={[
					{
						id: "applications",
						label: "Applications",
						content:
							tab !== "strategies" ? (
								<Applications
									scope={scope}
									enabled={enabled}
									onCreated={(id) => {
										onChanged();
										go(appconfigHref({ ...scope, application: id }));
									}}
								/>
							) : null,
					},
					{
						id: "strategies",
						label: "Deployment strategies",
						content:
							tab === "strategies" ? (
								<Strategies
									scope={scope}
									enabled={enabled}
									onChanged={onChanged}
								/>
							) : null,
					},
				]}
			/>
		);
	const app = application.data;
	if (!app)
		return (
			<Pending
				error={application.error}
				enabled={enabled}
				what="the application"
			/>
		);
	const toApplication = (next?: ApplicationTab) =>
		go(appconfigHref({ ...scope, application: app.Id, tab: next }));
	if (selected.environment) {
		const env = environment.data;
		if (!env)
			return (
				<Pending
					error={environment.error}
					enabled={enabled}
					what="the environment"
				/>
			);
		return (
			<EnvironmentDetails
				key={env.Id}
				scope={scope}
				application={app}
				environment={env}
				enabled={enabled}
				onChanged={() => {
					environment.reload();
					onChanged();
				}}
				onBack={() => toApplication("environments")}
				onDeleted={() => {
					onChanged();
					toApplication("environments");
				}}
			/>
		);
	}
	if (selected.profile) {
		const found = profile.data;
		if (!found)
			return (
				<Pending
					error={profile.error}
					enabled={enabled}
					what="the configuration profile"
				/>
			);
		return (
			<ProfileDetails
				key={found.Id}
				scope={scope}
				application={app}
				profile={found}
				enabled={enabled}
				onChanged={() => {
					profile.reload();
					onChanged();
				}}
				onBack={() => toApplication("profiles")}
				onDeleted={() => {
					onChanged();
					toApplication("profiles");
				}}
			/>
		);
	}
	return (
		<ApplicationDetails
			key={app.Id}
			scope={scope}
			application={app}
			enabled={enabled}
			tab={applicationTabs.find((t) => t === tab) ?? "environments"}
			onChanged={() => {
				application.reload();
				onChanged();
			}}
			onBack={() => go(appconfigHref(scope))}
			onDeleted={() => {
				onChanged();
				go(appconfigHref(scope));
			}}
		/>
	);
}
