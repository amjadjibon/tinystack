import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { ConsoleLayout } from "#/components/Console";
import { DomainDetails } from "#/components/opensearch/DomainDetails";
import { Domains } from "#/components/opensearch/Domains";
import type { Resource } from "#/lib/api";
import { type OpenSearchScope, opensearchHref } from "#/lib/opensearch";
import { runtimeHeader, runtimeNote } from "#/lib/opensearchHelpers";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";

export const Route = createFileRoute("/services/opensearch")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
		domain: String(search.domain ?? ""),
	}),
	component: OpenSearchPage,
});

function OpenSearchPage() {
	const search = Route.useSearch();
	const { canWrite, account, region, capabilities, inventory } = useScope();
	const enabled =
		!!capabilities.data?.pages.opensearch &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const scope = { account, region };
	const home = opensearchHref(scope);
	const runtime = capabilities.data?.opensearch_runtime;
	return (
		<ConsoleLayout
			contentType={search.domain ? "default" : "table"}
			navigationHeader={{ text: "OpenSearch Service", href: home }}
			activeHref={home}
			navigationItems={[
				{ type: "link", text: "Domains", href: home },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "OpenSearch Service", href: home },
				...(search.domain
					? [
							{
								text: search.domain,
								href: opensearchHref({ ...scope, domain: search.domain }),
							},
						]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{(!!capabilities.error || !!inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.opensearch && (
						<Alert type="warning">
							OpenSearch Service is not enabled on this server.
						</Alert>
					)}
					{!canWrite && (
						<Alert type="info">
							Connect a console session for this account to manage domains.
						</Alert>
					)}
					<Alert type="info" header={runtimeHeader(runtime)}>
						{runtimeNote(runtime)}
					</Alert>
					<Workspace
						key={`${account}/${region}/${enabled}`}
						scope={scope}
						enabled={enabled}
						domain={search.domain}
						runtime={runtime}
						resources={inventory.data ?? []}
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
	domain,
	runtime,
	resources,
	onChanged,
}: {
	scope: OpenSearchScope;
	enabled: boolean;
	domain: string;
	runtime?: string;
	resources: Resource[];
	onChanged: () => void;
}) {
	const navigate = useNavigate();
	if (domain)
		return (
			<DomainDetails
				key={domain}
				scope={scope}
				name={domain}
				enabled={enabled}
				runtime={runtime}
				resources={resources}
				onChanged={onChanged}
				onBack={() => navigate({ href: opensearchHref(scope) })}
			/>
		);
	return (
		<Domains
			scope={scope}
			enabled={enabled}
			onCreated={(name) => {
				onChanged();
				navigate({ href: opensearchHref({ ...scope, domain: name }) });
			}}
		/>
	);
}
