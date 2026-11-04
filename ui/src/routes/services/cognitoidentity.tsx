import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { ConsoleLayout } from "#/components/Console";
import { PoolDetails } from "#/components/cognitoidentity/PoolDetails";
import { PoolList } from "#/components/cognitoidentity/PoolList";
import { cognitoHref } from "#/lib/cognito";
import { identityHref } from "#/lib/cognitoidentity";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";

export const Route = createFileRoute("/services/cognitoidentity")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
		pool: String(search.pool ?? ""),
	}),
	component: IdentityPoolsPage,
});

function IdentityPoolsPage() {
	const search = Route.useSearch(),
		{ account, region, canWrite, capabilities, inventory } = useScope();
	const navigate = useNavigate();
	const scope = { account, region };
	const home = identityHref(scope);
	const enabled =
		!!capabilities.data?.pages.cognitoidentity &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	return (
		<ConsoleLayout
			contentType={search.pool ? "default" : "table"}
			navigationHeader={{ text: "Cognito identity pools", href: home }}
			activeHref={home}
			navigationItems={[
				{ type: "link", text: "Identity pools", href: home },
				{
					type: "link",
					text: "User pools",
					href: cognitoHref({ account, region }),
				},
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "Cognito identity pools", href: home },
				...(search.pool
					? [{ text: search.pool, href: identityHref(scope, search.pool) }]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{!!(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.cognitoidentity && (
						<Alert type="warning">
							Cognito identity pools is not enabled on this server.
						</Alert>
					)}
					{!canWrite && (
						<Alert type="info">
							Connect a console session for this account to manage identity
							pools.
						</Alert>
					)}
					{search.pool ? (
						<PoolDetails
							key={`${account}/${region}/${search.pool}/${enabled}`}
							scope={scope}
							pool={search.pool}
							enabled={enabled}
							onChanged={inventory.reload}
							onDeleted={() => {
								inventory.reload();
								navigate({ href: home });
							}}
						/>
					) : (
						<PoolList
							key={`${account}/${region}/${enabled}`}
							scope={scope}
							enabled={enabled}
							onChanged={inventory.reload}
						/>
					)}
				</SpaceBetween>
			}
		/>
	);
}
