import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { ConsoleLayout } from "#/components/Console";
import { CloudMap } from "#/components/servicediscovery/CloudMap";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { cloudMapHref } from "#/lib/servicediscovery";

export const Route = createFileRoute("/services/servicediscovery")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
	}),
	component: CloudMapPage,
});

function CloudMapPage() {
	const { canWrite, account, region, capabilities, inventory } = useScope();
	const enabled =
		!!capabilities.data?.pages.servicediscovery &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const scope = { account, region };
	const home = cloudMapHref(scope);
	return (
		<ConsoleLayout
			navigationHeader={{ text: "Cloud Map", href: home }}
			activeHref={home}
			navigationItems={[
				{ type: "link", text: "Namespaces", href: home },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[{ text: "Cloud Map", href: home }]}
			content={
				<SpaceBetween size="l">
					{!!(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.servicediscovery && (
						<Alert type="warning">
							Cloud Map is not enabled on this server.
						</Alert>
					)}
					{!canWrite && (
						<Alert type="info">
							Connect a console session for this account to manage resources.
						</Alert>
					)}
					<CloudMap
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
