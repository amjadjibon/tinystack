import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { ConsoleLayout } from "#/components/Console";
import { GlueConsole } from "#/components/glue/GlueConsole";
import { glueHref } from "#/lib/glue";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";

export const Route = createFileRoute("/services/glue")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
	}),
	component: GluePage,
});

function GluePage() {
	const { canWrite, account, region, capabilities, inventory } = useScope();
	const enabled =
		!!capabilities.data?.pages.glue &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const scope = { account, region };
	const home = glueHref(scope);
	return (
		<ConsoleLayout
			navigationHeader={{ text: "AWS Glue", href: home }}
			activeHref={home}
			navigationItems={[
				{ type: "link", text: "Glue", href: home },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[{ text: "AWS Glue", href: home }]}
			content={
				<SpaceBetween size="l">
					{!!(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.glue && (
						<Alert type="warning">
							AWS Glue is not enabled on this server.
						</Alert>
					)}
					{!canWrite && (
						<Alert type="info">
							Connect a console session for this account to manage resources.
						</Alert>
					)}
					<GlueConsole
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
