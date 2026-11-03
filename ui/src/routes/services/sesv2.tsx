import { Alert, SpaceBetween, Tabs } from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { useState } from "react";
import { ConsoleLayout } from "#/components/Console";
import { Facts } from "#/components/sesv2/Facts";
import { SESConfigSets } from "#/components/sesv2/SESConfigSets";
import { SESIdentities } from "#/components/sesv2/SESIdentities";
import { SESMessages } from "#/components/sesv2/SESMessages";
import { SESSend } from "#/components/sesv2/SESSend";
import { SESTemplates } from "#/components/sesv2/SESTemplates";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import {
	getAccount,
	listConfigSets,
	listIdentities,
	listTemplates,
	type SESScope,
	sesv2Href,
} from "#/lib/sesv2";
import { quotaText } from "#/lib/sesv2Helpers";
import { usePoll } from "#/lib/usePoll";

export const Route = createFileRoute("/services/sesv2")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
	}),
	component: SESv2Page,
});

function SESv2Page() {
	const { canWrite, account, region, capabilities, inventory } = useScope();
	const enabled =
		!!capabilities.data?.pages.sesv2 &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const scope = { account, region };
	const home = sesv2Href(scope);
	return (
		<ConsoleLayout
			navigationHeader={{ text: "Simple Email Service", href: home }}
			activeHref={home}
			navigationItems={[
				{ type: "link", text: "Email sending", href: home },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[{ text: "Simple Email Service", href: home }]}
			content={
				<SpaceBetween size="l">
					{(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.sesv2 && (
						<Alert type="warning">
							Simple Email Service is not enabled on this server.
						</Alert>
					)}
					{!canWrite && (
						<Alert type="info">
							Connect a console session for this account to manage resources.
						</Alert>
					)}
					<Workspace
						key={`${account}/${region}/${enabled}`}
						scope={scope}
						enabled={enabled}
						onChanged={inventory.reload}
					/>
				</SpaceBetween>
			}
		/>
	);
}

function Workspace({
	scope,
	enabled,
	onChanged,
}: {
	scope: SESScope;
	enabled: boolean;
	onChanged: () => void;
}) {
	const [tab, setTab] = useState("identities");
	const identities = usePoll(
		() => (enabled ? listIdentities(scope) : Promise.resolve([])),
		0,
	);
	const configSets = usePoll(
		() => (enabled ? listConfigSets(scope) : Promise.resolve([])),
		0,
	);
	const templates = usePoll(
		() => (enabled ? listTemplates(scope) : Promise.resolve([])),
		0,
	);
	const account = usePoll(
		() => (enabled ? getAccount(scope) : Promise.resolve(undefined)),
		0,
	);
	const refresh = () => {
		identities.reload();
		configSets.reload();
		templates.reload();
		account.reload();
		onChanged();
	};
	const a = account.data;
	return (
		<SpaceBetween size="m">
			{[identities.error, configSets.error, templates.error, account.error]
				.filter((e) => e)
				.map((e) => (
					<Alert key={e} type="error">
						{e}
					</Alert>
				))}
			<Facts
				title="Account"
				facts={[
					["Sending", a ? (a.SendingEnabled ? "Enabled" : "Paused") : ""],
					["Enforcement status", a?.EnforcementStatus],
					[
						"Production access",
						a ? (a.ProductionAccessEnabled ? "Granted" : "Sandbox") : "",
					],
					["Send quota", quotaText(a?.SendQuota)],
				]}
			/>
			<Tabs
				activeTabId={tab}
				onChange={({ detail }) => setTab(detail.activeTabId)}
				tabs={[
					{
						id: "identities",
						label: "Identities",
						content: (
							<SESIdentities
								scope={scope}
								enabled={enabled}
								identities={identities.data ?? []}
								configSets={configSets.data ?? []}
								loading={identities.loading}
								onChanged={refresh}
							/>
						),
					},
					{
						id: "sets",
						label: "Configuration sets",
						content: (
							<SESConfigSets
								scope={scope}
								enabled={enabled}
								configSets={configSets.data ?? []}
								loading={configSets.loading}
								onChanged={refresh}
							/>
						),
					},
					{
						id: "templates",
						label: "Templates",
						content: (
							<SESTemplates
								scope={scope}
								enabled={enabled}
								templates={templates.data ?? []}
								loading={templates.loading}
								onChanged={refresh}
							/>
						),
					},
					{
						id: "send",
						label: "Send test email",
						content: (
							<SESSend
								scope={scope}
								enabled={enabled}
								identities={identities.data ?? []}
								templates={templates.data ?? []}
								configSets={configSets.data ?? []}
								onSent={account.reload}
							/>
						),
					},
					{
						id: "messages",
						label: "Sent messages",
						content:
							tab === "messages" ? (
								<SESMessages scope={scope} enabled={enabled} />
							) : null,
					},
				]}
			/>
		</SpaceBetween>
	);
}
