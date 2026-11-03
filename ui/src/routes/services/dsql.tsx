import { Alert, Link, SpaceBetween, Tabs } from "@cloudscape-design/components";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { ConsoleLayout } from "#/components/Console";
import { ConsoleTags } from "#/components/ConsoleTags";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { ClusterDialog } from "#/components/dsql/ClusterDialog";
import { ClusterStatus, Details } from "#/components/dsql/Details";
import { clusters, dsqlCall, tagPlan } from "#/lib/dsql";
import {
	type DSQLScope,
	dsqlHref,
	editable,
	needsPeer,
	peerArns,
	pending,
} from "#/lib/dsqlHelpers";
import { unixTime } from "#/lib/format";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";

export const Route = createFileRoute("/services/dsql")({
	validateSearch: (s: Record<string, unknown>) => ({
		account: String(s.account ?? ""),
		region: String(s.region ?? ""),
		cluster: String(s.cluster ?? ""),
		tab: s.tab === "tags" ? "tags" : "overview",
	}),
	component: DSQLPage,
});

function DSQLPage() {
	const { account, region, canWrite, capabilities, inventory } = useScope();
	const search = Route.useSearch(),
		scope = { account, region },
		home = dsqlHref(scope);
	const enabled =
		!!capabilities.data?.pages.dsql &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const docker = capabilities.data?.dsql_runtime === "docker";
	return (
		<ConsoleLayout
			contentType={search.cluster ? "default" : "table"}
			navigationHeader={{ text: "Amazon Aurora DSQL", href: home }}
			activeHref={home}
			navigationItems={[
				{ type: "link", text: "Clusters", href: home },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "Amazon Aurora DSQL", href: home },
				...(search.cluster
					? [{ text: search.cluster, href: dsqlHref(scope, search.cluster) }]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{!!(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.dsql && (
						<Alert type="warning">
							Amazon Aurora DSQL is not enabled on this server.
						</Alert>
					)}
					{!canWrite && (
						<Alert>
							Connect a console session for this account to manage resources.
						</Alert>
					)}
					<Alert
						header={
							docker ? "Docker runtime configured" : "Records-only runtime"
						}
					>
						{docker
							? "Each cluster runs one PostgreSQL 16 container behind a TLS proxy on a loopback port. The page refreshes while clusters change. Once a cluster is ACTIVE, connect with psql and an IAM auth token as shown in its details. This is PostgreSQL, not the Aurora DSQL engine, and clusters paired across Regions share one local database."
							: "Clusters are stored as records. No PostgreSQL database runs, so endpoints are placeholders that accept no connections. Set dsql.clusters to docker for working PostgreSQL clusters."}
					</Alert>
					<Workspace
						key={`${account}/${region}/${enabled}/${search.cluster}`}
						scope={scope}
						enabled={enabled}
						docker={docker}
						id={search.cluster}
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
	docker,
	id,
	tab,
	onChanged,
}: {
	scope: DSQLScope;
	enabled: boolean;
	docker: boolean;
	id: string;
	tab: string;
	onChanged: () => void;
}) {
	const navigate = useNavigate(),
		follow = useFollow();
	const [changing, setChanging] = useState(true),
		[dialog, setDialog] = useState<"" | "create" | "edit" | "delete">("");
	const poll = usePoll(
		async () => {
			const rows = enabled ? await clusters(scope) : [];
			setChanging(rows.some((c) => pending(c.Status)));
			return rows;
		},
		changing ? 2000 : 0,
	);
	const c = poll.data?.find((c) => c.Identifier === id);
	const saved = () => {
		setDialog("");
		setChanging(true);
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{!id ? (
				<Rows
					title="Clusters"
					items={poll.data ?? []}
					loading={poll.loading}
					emptyText="Choose Create cluster to add one."
					actions={
						<Buttons>
							<Action onClick={poll.reload}>Refresh</Action>
							<Action disabled={!enabled} onClick={() => setDialog("create")}>
								Create cluster
							</Action>
						</Buttons>
					}
					columns={[
						{
							id: "identifier",
							header: "Identifier",
							cell: (c) => (
								<Link href={dsqlHref(scope, c.Identifier)} onFollow={follow}>
									{c.Identifier}
								</Link>
							),
						},
						{
							id: "status",
							header: "Status",
							cell: (c) => <ClusterStatus status={c.Status} />,
						},
						{
							id: "created",
							header: "Created",
							cell: (c) => unixTime(c.CreationTime),
						},
						{
							id: "protection",
							header: "Deletion protection",
							cell: (c) => (c.DeletionProtectionEnabled ? "On" : "Off"),
						},
					]}
				/>
			) : c ? (
				<SpaceBetween size="m">
					<Buttons>
						<Action onClick={() => navigate({ href: dsqlHref(scope) })}>
							Clusters
						</Action>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action
							disabled={!enabled || !editable(c.Status)}
							onClick={() => setDialog("edit")}
						>
							Edit settings
						</Action>
						<Action
							disabled={
								!enabled || !editable(c.Status) || !!c.DeletionProtectionEnabled
							}
							onClick={() => setDialog("delete")}
						>
							Delete cluster
						</Action>
					</Buttons>
					{!!c.DeletionProtectionEnabled && editable(c.Status) && (
						<Alert header="Deletion protection is on">
							This cluster cannot be deleted. Choose Edit settings and turn
							deletion protection off first.
						</Alert>
					)}
					<Tabs
						activeTabId={tab}
						onChange={({ detail }) =>
							navigate({ href: dsqlHref(scope, id, detail.activeTabId) })
						}
						tabs={[
							{
								id: "overview",
								label: "Overview",
								content:
									tab === "overview" ? (
										<Details
											key={`${c.Arn}/${c.Status}`}
											scope={scope}
											cluster={c}
											docker={docker}
										/>
									) : null,
							},
							{
								id: "tags",
								label: "Tags",
								content:
									tab === "tags" ? (
										<ConsoleTags
											tags={c.Tags ?? {}}
											enabled={enabled && editable(c.Status)}
											onChanged={saved}
											plan={tagPlan(scope, c.Arn)}
										/>
									) : null,
							},
						]}
					/>
				</SpaceBetween>
			) : (
				!poll.loading && (
					<Alert>Cluster not found in this account and Region.</Alert>
				)
			)}
			{(dialog === "create" || (dialog === "edit" && !!c)) && (
				<ClusterDialog
					scope={scope}
					cluster={dialog === "create" ? undefined : c}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
			{dialog === "delete" && !!c && (
				<ActionDialog
					title="Delete cluster"
					submit="Delete cluster"
					confirmation={c.Identifier}
					onClose={() => setDialog("")}
					onSaved={() => {
						saved();
						navigate({ href: dsqlHref(scope) });
					}}
					plan={async () => [
						{
							label: "DeleteCluster",
							run: () =>
								dsqlCall(scope, "DeleteCluster", { Identifier: c.Identifier }),
						},
					]}
				>
					<SpaceBetween size="m">
						<Alert type="warning">
							Delete {c.Identifier} permanently
							{docker
								? ", including its PostgreSQL container and data volume once no peer cluster shares them"
								: ""}
							.
						</Alert>
						{!!c.MultiRegionProperties && !needsPeer(c) && (
							<Alert>
								This cluster is paired with {peerArns(c).join(", ")}. It stays
								PENDING_DELETE until the peer is deleted too.
							</Alert>
						)}
					</SpaceBetween>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
