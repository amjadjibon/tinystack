import { Alert, Box, SpaceBetween } from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { ConsoleLayout } from "#/components/Console";
import { CacheClusters } from "#/components/elasticache/CacheClusters";
import {
	CacheEvents,
	ParameterGroups,
	SubnetGroups,
} from "#/components/elasticache/CacheGroups";
import { useCacheOptions } from "#/components/elasticache/CacheKit";
import { ReplicationGroups } from "#/components/elasticache/ReplicationGroups";
import { ServerlessCaches } from "#/components/elasticache/ServerlessCaches";
import { elasticacheHref, runtimeNote } from "#/lib/elasticache";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";

const sections = [
	{ id: "clusters", text: "Cache clusters" },
	{ id: "groups", text: "Replication groups" },
	{ id: "serverless", text: "Serverless caches" },
	{ id: "subnets", text: "Subnet groups" },
	{ id: "parameters", text: "Parameter groups" },
	{ id: "events", text: "Events" },
];

export const Route = createFileRoute("/services/elasticache")({
	validateSearch: (s: Record<string, unknown>) => ({
		account: String(s.account ?? ""),
		region: String(s.region ?? ""),
		section: sections.some((x) => x.id === s.section)
			? String(s.section)
			: "clusters",
	}),
	component: ElastiCachePage,
});

function ElastiCachePage() {
	const search = Route.useSearch(),
		{ canWrite, account, region, capabilities, inventory } = useScope();
	const scope = { account, region, section: search.section };
	const enabled =
		!!capabilities.data?.elasticache &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const runtime = capabilities.data?.elasticache_runtime;
	const current = sections.find((s) => s.id === search.section) ?? sections[0];
	return (
		<ConsoleLayout
			navigationHeader={{
				text: "ElastiCache",
				href: elasticacheHref({ account, region }),
			}}
			activeHref={elasticacheHref({
				account,
				region,
				section: search.section,
			})}
			navigationItems={[
				...sections.map((s) => ({
					type: "link" as const,
					text: s.text,
					href: elasticacheHref({ account, region, section: s.id }),
				})),
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "ElastiCache", href: elasticacheHref({ account, region }) },
				{
					text: current.text,
					href: elasticacheHref({ account, region, section: current.id }),
				},
			]}
			content={
				<SpaceBetween size="l">
					{capabilities.error && (
						<Alert type="error">{capabilities.error}</Alert>
					)}
					{capabilities.data?.requires_signature && !canWrite && (
						<Alert type="info">
							Connect a root session for the selected account using Console
							session in the top bar, or use signed AWS CLI/SDK requests.
						</Alert>
					)}
					{capabilities.data && !capabilities.data.elasticache && (
						<Alert type="warning">
							ElastiCache is not enabled on this server.
						</Alert>
					)}
					<Alert
						type="info"
						header={
							runtime === "docker"
								? "Docker runtime configured"
								: "Records-only runtime"
						}
					>
						{runtimeNote(runtime)}
					</Alert>
					<Workspace
						key={`${account}/${region}/${search.section}/${enabled}`}
						scope={scope}
						enabled={enabled}
						fallbackCount={
							(inventory.data ?? []).filter((r) => r.service === "elasticache")
								.length
						}
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
	fallbackCount,
	onChanged,
}: {
	scope: { account: string; region: string; section: string };
	enabled: boolean;
	fallbackCount: number;
	onChanged: () => void;
}) {
	const options = useCacheOptions(scope, enabled);
	if (!enabled)
		return (
			<Box>
				{fallbackCount} ElastiCache resources are recorded in this scope.
				Connect a console session to manage them.
			</Box>
		);
	if (options.error) return <Alert type="error">{options.error}</Alert>;
	if (!options.data) return <Box>Loading ElastiCache options…</Box>;
	const props = { scope, enabled, options: options.data, onChanged };
	switch (scope.section) {
		case "groups":
			return <ReplicationGroups {...props} />;
		case "serverless":
			return <ServerlessCaches {...props} />;
		case "subnets":
			return <SubnetGroups {...props} />;
		case "parameters":
			return <ParameterGroups {...props} />;
		case "events":
			return <CacheEvents scope={scope} enabled={enabled} />;
		default:
			return <CacheClusters {...props} />;
	}
}
