import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { AthenaWorkspace } from "#/components/athena/AthenaWorkspace";
import { ConsoleLayout } from "#/components/Console";
import { athenaHref } from "#/lib/athena";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";

export const Route = createFileRoute("/services/athena")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
	}),
	component: AthenaPage,
});

function AthenaPage() {
	const { canWrite, account, region, capabilities, inventory } = useScope();
	const enabled =
		!!capabilities.data?.pages.athena &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const scope = { account, region };
	const home = athenaHref(scope);
	return (
		<ConsoleLayout
			navigationHeader={{ text: "Athena", href: home }}
			activeHref={home}
			navigationItems={[
				{ type: "link", text: "Query editor", href: home },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[{ text: "Athena", href: home }]}
			content={
				<SpaceBetween size="l">
					{!!(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.athena && (
						<Alert type="warning">Athena is not enabled on this server.</Alert>
					)}
					{!canWrite && (
						<Alert type="info">
							Connect a console session for this account to manage resources.
						</Alert>
					)}
					<AthenaWorkspace
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
