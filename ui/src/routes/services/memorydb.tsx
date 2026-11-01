import { Alert, SpaceBetween, Tabs } from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { useState } from "react";
import { ConsoleLayout } from "#/components/Console";
import {
	MemoryDBACLs,
	MemoryDBUsers,
} from "#/components/memorydb/MemoryDBAccess";
import { MemoryDBClusters } from "#/components/memorydb/MemoryDBClusters";
import {
	MemoryDBSubnetGroups,
	MemoryDBTags,
} from "#/components/memorydb/MemoryDBSubnetsAndTags";
import {
	listACLs,
	listSubnetGroups,
	listUsers,
	type MemoryDBScope,
	memorydbHref,
} from "#/lib/memorydb";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { usePoll } from "#/lib/usePoll";

export const Route = createFileRoute("/services/memorydb")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
	}),
	component: MemoryDBPage,
});

function MemoryDBPage() {
	const { canWrite, account, region, capabilities, inventory } = useScope();
	const enabled =
		!!capabilities.data?.memorydb &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const scope = { account, region };
	const home = memorydbHref(scope);
	const docker = capabilities.data?.memorydb_runtime === "docker";
	return (
		<ConsoleLayout
			navigationHeader={{ text: "MemoryDB", href: home }}
			activeHref={home}
			navigationItems={[
				{ type: "link", text: "Clusters", href: home },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[{ text: "MemoryDB", href: home }]}
			content={
				<SpaceBetween size="l">
					{(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.memorydb && (
						<Alert type="warning">
							MemoryDB is not enabled on this server.
						</Alert>
					)}
					{!canWrite && (
						<Alert type="info">
							Connect a console session for this account to manage resources.
						</Alert>
					)}
					<Alert
						type="info"
						header={
							docker ? "Docker runtime configured" : "Records-only runtime"
						}
					>
						{docker
							? "Each cluster runs one Valkey or Redis OSS engine in Docker. Creation reports creating until the engine is ready, and the page refreshes the status while a cluster changes."
							: "Clusters, users and ACLs are stored as records. No engine runs, so the endpoint is a placeholder name. Set memorydb.clusters to docker for a working engine."}
					</Alert>
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
	scope: MemoryDBScope;
	enabled: boolean;
	onChanged: () => void;
}) {
	const [tab, setTab] = useState("clusters");
	const users = usePoll(
		() => (enabled ? listUsers(scope) : Promise.resolve([])),
		0,
	);
	const acls = usePoll(
		() => (enabled ? listACLs(scope) : Promise.resolve([])),
		0,
	);
	const groups = usePoll(
		() => (enabled ? listSubnetGroups(scope) : Promise.resolve([])),
		0,
	);
	const refresh = () => {
		users.reload();
		acls.reload();
		groups.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="m">
			{[users.error, acls.error, groups.error]
				.filter((e) => e)
				.map((e) => (
					<Alert key={e} type="error">
						{e}
					</Alert>
				))}
			<Tabs
				activeTabId={tab}
				onChange={({ detail }) => setTab(detail.activeTabId)}
				tabs={[
					{
						id: "clusters",
						label: "Clusters",
						content: (
							<MemoryDBClusters
								scope={scope}
								enabled={enabled}
								acls={acls.data ?? []}
								subnetGroups={groups.data ?? []}
								onChanged={refresh}
							/>
						),
					},
					{
						id: "users",
						label: "Users",
						content: (
							<MemoryDBUsers
								scope={scope}
								enabled={enabled}
								users={users.data ?? []}
								loading={users.loading}
								onChanged={refresh}
							/>
						),
					},
					{
						id: "acls",
						label: "Access control lists",
						content: (
							<MemoryDBACLs
								scope={scope}
								enabled={enabled}
								acls={acls.data ?? []}
								users={users.data ?? []}
								loading={acls.loading}
								onChanged={refresh}
							/>
						),
					},
					{
						id: "subnets",
						label: "Subnet groups",
						content: (
							<MemoryDBSubnetGroups
								scope={scope}
								enabled={enabled}
								groups={groups.data ?? []}
								loading={groups.loading}
								onChanged={refresh}
							/>
						),
					},
					{
						id: "tags",
						label: "Tags",
						content:
							tab === "tags" ? (
								<MemoryDBTags
									scope={scope}
									enabled={enabled}
									users={users.data ?? []}
									acls={acls.data ?? []}
									groups={groups.data ?? []}
								/>
							) : null,
					},
				]}
			/>
		</SpaceBetween>
	);
}
