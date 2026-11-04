import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { ApiDetails } from "#/components/appsync/ApiDetails";
import { Resources } from "#/components/appsync/Resources";
import { ConsoleLayout } from "#/components/Console";
import { appsyncHref } from "#/lib/appsyncHelpers";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";

export const Route = createFileRoute("/services/appsync")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
		appsyncApi: String(search.appsyncApi ?? ""),
	}),
	component: AppSyncPage,
});
function AppSyncPage() {
	const search = Route.useSearch(),
		navigate = useNavigate();
	const { account, region, canWrite, capabilities, inventory } = useScope();
	const scope = { account, region },
		home = appsyncHref(scope);
	const enabled =
		!!capabilities.data?.pages.appsync &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	return (
		<ConsoleLayout
			contentType={search.appsyncApi ? "default" : "table"}
			navigationHeader={{ text: "AWS AppSync", href: home }}
			activeHref={home}
			navigationItems={[
				{ type: "link", text: "GraphQL APIs", href: home },
				{ type: "link", text: "All services", href: "/services" },
			]}
			breadcrumbs={[
				{ text: "AppSync", href: home },
				...(search.appsyncApi
					? [
							{
								text: search.appsyncApi,
								href: appsyncHref(scope, search.appsyncApi),
							},
						]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{!!(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.appsync && (
						<Alert type="warning">AppSync is not enabled on this server.</Alert>
					)}
					{!canWrite && (
						<Alert type="info">
							Connect a console session for this account to manage AppSync APIs.
						</Alert>
					)}
					<div key={`${account}/${region}/${enabled}/${search.appsyncApi}`}>
						{search.appsyncApi ? (
							<ApiDetails
								scope={scope}
								apiId={search.appsyncApi}
								enabled={enabled}
								onChanged={inventory.reload}
								onDeleted={() => navigate({ href: home })}
							/>
						) : (
							<Resources
								scope={scope}
								kind="api"
								enabled={enabled}
								onChanged={inventory.reload}
							/>
						)}
					</div>
				</SpaceBetween>
			}
		/>
	);
}
