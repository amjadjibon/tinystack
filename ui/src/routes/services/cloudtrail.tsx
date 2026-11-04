import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { ConsoleLayout } from "#/components/Console";
import { CloudTrailWorkspace } from "#/components/cloudtrail/CloudTrailWorkspace";
import { cloudtrailHref } from "#/lib/cloudtrail";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";

export const Route = createFileRoute("/services/cloudtrail")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
	}),
	component: CloudTrailPage,
});

function CloudTrailPage() {
	const { canWrite, account, region, capabilities, inventory } = useScope();
	const enabled =
		!!capabilities.data?.pages.cloudtrail &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const scope = { account, region };
	const home = cloudtrailHref(scope);
	return (
		<ConsoleLayout
			navigationHeader={{ text: "CloudTrail", href: home }}
			activeHref={home}
			navigationItems={[
				{ type: "link", text: "Event history and trails", href: home },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[{ text: "CloudTrail", href: home }]}
			content={
				<SpaceBetween size="l">
					{!!(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.cloudtrail && (
						<Alert type="warning">
							CloudTrail is not enabled on this server.
						</Alert>
					)}
					{!canWrite && (
						<Alert type="info">
							Connect a console session for this account to manage resources.
						</Alert>
					)}
					<CloudTrailWorkspace
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
