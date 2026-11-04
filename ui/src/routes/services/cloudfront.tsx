import { Alert, SpaceBetween, Tabs } from "@cloudscape-design/components";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { ConsoleLayout } from "#/components/Console";
import { DistributionDetails } from "#/components/cloudfront/DistributionDetails";
import { Distributions } from "#/components/cloudfront/Distributions";
import { KeyValueStores } from "#/components/cloudfront/KeyValueStores";
import { OriginAccessControls } from "#/components/cloudfront/OriginAccessControls";
import {
	type CloudFrontScope,
	type CloudFrontTab,
	cloudfrontHref,
	cloudfrontTabs,
	listOacs,
} from "#/lib/cloudfront";
import { useScope } from "#/lib/scope";
import { usePoll } from "#/lib/usePoll";

export const Route = createFileRoute("/services/cloudfront")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		distribution: String(search.distribution ?? ""),
		tab: cloudfrontTabs.find((t) => t === search.tab) ?? "distributions",
	}),
	component: CloudFrontPage,
});

function CloudFrontPage() {
	const search = Route.useSearch();
	const { canWrite, account, capabilities, inventory } = useScope();
	const enabled =
		!!capabilities.data?.pages.cloudfront &&
		canWrite &&
		/^\d{12}$/.test(account);
	const scope = { account };
	const home = cloudfrontHref(scope);
	return (
		<ConsoleLayout
			contentType={search.distribution ? "default" : "table"}
			navigationHeader={{ text: "CloudFront", href: home }}
			activeHref={home}
			navigationItems={[
				{ type: "link", text: "Distributions", href: home },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "CloudFront", href: home },
				...(search.distribution
					? [
							{
								text: search.distribution,
								href: cloudfrontHref({
									account,
									distribution: search.distribution,
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
					{capabilities.data && !capabilities.data.pages.cloudfront && (
						<Alert type="warning">
							CloudFront is not enabled on this server.
						</Alert>
					)}
					{capabilities.data?.requires_signature && !canWrite && (
						<Alert type="info">
							Connect a root console session for the selected account to manage
							CloudFront.
						</Alert>
					)}
					<Workspace
						key={`${account}/${enabled}`}
						scope={scope}
						enabled={enabled}
						distribution={search.distribution}
						tab={search.tab}
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
	distribution,
	tab,
	onChanged,
}: {
	scope: CloudFrontScope;
	enabled: boolean;
	distribution: string;
	tab: CloudFrontTab;
	onChanged: () => void;
}) {
	const navigate = useNavigate();
	// Origins pick from the access controls, and the access control tab lists them.
	const oacs = usePoll(() => (enabled ? listOacs(scope) : Promise.resolve([])));
	const refresh = () => {
		oacs.reload();
		onChanged();
	};
	if (distribution)
		return (
			<DistributionDetails
				key={distribution}
				scope={scope}
				id={distribution}
				enabled={enabled}
				oacs={oacs.data ?? []}
				onChanged={refresh}
				onBack={() => navigate({ href: cloudfrontHref(scope) })}
			/>
		);
	return (
		<Tabs
			activeTabId={tab}
			onChange={({ detail }) =>
				navigate({
					href: cloudfrontHref({
						...scope,
						tab: detail.activeTabId as CloudFrontTab,
					}),
				})
			}
			tabs={[
				{
					id: "distributions",
					label: "Distributions",
					content:
						tab === "distributions" ? (
							<Distributions
								scope={scope}
								enabled={enabled}
								oacs={oacs.data ?? []}
								onCreated={(id) => {
									refresh();
									navigate({
										href: cloudfrontHref({ ...scope, distribution: id }),
									});
								}}
							/>
						) : null,
				},
				{
					id: "oacs",
					label: "Origin access controls",
					content:
						tab === "oacs" ? (
							<OriginAccessControls
								scope={scope}
								enabled={enabled}
								items={oacs.data ?? []}
								loading={oacs.loading}
								error={oacs.error}
								onChanged={refresh}
							/>
						) : null,
				},
				{
					id: "kvs",
					label: "Key value stores",
					content:
						tab === "kvs" ? (
							<KeyValueStores
								scope={scope}
								enabled={enabled}
								onChanged={onChanged}
							/>
						) : null,
				},
			]}
		/>
	);
}
