import { Alert, Link, SpaceBetween, Tabs } from "@cloudscape-design/components";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { ConsoleLayout } from "#/components/Console";
import { ConsoleTags } from "#/components/ConsoleTags";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { ClusterDialog } from "#/components/kafka/ClusterDialog";
import { Details } from "#/components/kafka/Details";
import { isoTime } from "#/lib/format";
import { kafkaCall, kafkaList } from "#/lib/kafka";
import {
	type Cluster,
	createRegions,
	type KafkaScope,
	kafkaHref,
	pending,
} from "#/lib/kafkaHelpers";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { tagList, tagMap } from "#/lib/tags";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
export const Route = createFileRoute("/services/kafka")({
	validateSearch: (s: Record<string, unknown>) => ({
		account: String(s.account ?? ""),
		region: String(s.region ?? ""),
		cluster: String(s.cluster ?? ""),
		tab: s.tab === "tags" ? "tags" : "overview",
	}),
	component: KafkaPage,
});
function KafkaPage() {
	const { account, region, canWrite, capabilities, inventory } = useScope();
	const search = Route.useSearch(),
		scope = { account, region },
		home = kafkaHref(scope);
	const enabled =
		!!capabilities.data?.pages.kafka &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const docker = capabilities.data?.kafka_runtime === "docker";
	return (
		<ConsoleLayout
			navigationHeader={{ text: "MSK", href: home }}
			activeHref={home}
			navigationItems={[
				{ type: "link", text: "Clusters", href: home },
				{ type: "link", text: "All services", href: "/services" },
			]}
			breadcrumbs={[
				{ text: "MSK", href: home },
				...(search.cluster
					? [{ text: search.cluster, href: kafkaHref(scope, search.cluster) }]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{!!(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.kafka && (
						<Alert type="warning">MSK is not enabled on this server.</Alert>
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
							? "Each cluster runs one Kafka-compatible Redpanda broker. Use the returned loopback bootstrap endpoints once ACTIVE. TLS uses the TinyStack local CA. Kafka version and broker count are metadata; no ZooKeeper runs."
							: "Clusters are stored as records. No Kafka brokers run, so bootstrap DNS names are placeholders. Set kafka.clusters to docker for working brokers. Authentication flags are retained without enforcing credentials."}
					</Alert>
					<Workspace
						key={`${account}/${region}/${enabled}/${search.cluster}`}
						scope={scope}
						enabled={enabled}
						docker={docker}
						name={search.cluster}
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
	name,
	tab,
	onChanged,
}: {
	scope: KafkaScope;
	enabled: boolean;
	docker: boolean;
	name: string;
	tab: string;
	onChanged: () => void;
}) {
	const navigate = useNavigate(),
		follow = useFollow();
	const [changing, setChanging] = useState(true),
		[dialog, setDialog] = useState("");
	const poll = usePoll(
		async () => {
			const rows = enabled
				? await kafkaList<Cluster>(scope, "ListClusters", "ClusterInfoList")
				: [];
			const c = name ? rows.find((c) => c.ClusterName === name) : undefined;
			if (c) {
				const out = await kafkaCall<{ ClusterInfo: Cluster }>(
					scope,
					"DescribeCluster",
					{ ClusterArn: c.ClusterArn },
				);
				rows[rows.indexOf(c)] = out.ClusterInfo;
			}
			setChanging(rows.some((c) => pending(c.State)));
			return rows;
		},
		changing ? 2000 : 0,
	);
	const c = poll.data?.find((c) => c.ClusterName === name);
	const saved = () => {
		setDialog("");
		setChanging(true);
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{!name ? (
				<Rows
					title="Clusters"
					items={poll.data ?? []}
					loading={poll.loading}
					emptyText="Choose Create cluster to add one."
					actions={
						<Buttons>
							<Action onClick={poll.reload}>Refresh</Action>
							<Action
								disabled={!enabled || !createRegions.includes(scope.region)}
								onClick={() => setDialog("create")}
							>
								Create cluster
							</Action>
						</Buttons>
					}
					columns={[
						{
							id: "name",
							header: "Name",
							cell: (c) => (
								<Link href={kafkaHref(scope, c.ClusterName)} onFollow={follow}>
									{c.ClusterName}
								</Link>
							),
						},
						{ id: "type", header: "Type", cell: () => "Provisioned" },
						{
							id: "version",
							header: "Kafka version",
							cell: (c) => c.CurrentBrokerSoftwareInfo?.KafkaVersion ?? "-",
						},
						{ id: "state", header: "State", cell: (c) => c.State ?? "-" },
						{
							id: "count",
							header: "Brokers",
							cell: (c) => c.NumberOfBrokerNodes ?? "-",
						},
						{
							id: "created",
							header: "Created",
							cell: (c) => isoTime(c.CreationTime),
						},
					]}
				/>
			) : c ? (
				<SpaceBetween size="m">
					<Buttons>
						<Action onClick={() => navigate({ href: kafkaHref(scope) })}>
							Clusters
						</Action>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action
							disabled={!enabled || pending(c.State)}
							onClick={() => setDialog("monitor")}
						>
							Edit monitoring
						</Action>
						<Action
							disabled={!enabled || pending(c.State)}
							onClick={() => setDialog("delete")}
						>
							Delete cluster
						</Action>
					</Buttons>
					<Tabs
						activeTabId={tab}
						onChange={({ detail }) =>
							navigate({ href: kafkaHref(scope, name, detail.activeTabId) })
						}
						tabs={[
							{
								id: "overview",
								label: "Overview",
								content:
									tab === "overview" ? (
										<Details
											key={`${c.ClusterArn}/${c.State}/${c.CurrentVersion}`}
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
											tags={tagMap(tagList(c.Tags ?? {}))}
											enabled={enabled && !pending(c.State)}
											onChanged={saved}
											plan={(next, removed) => [
												...(Object.keys(next).length
													? [
															{
																label: "TagResource",
																run: () =>
																	kafkaCall(scope, "TagResource", {
																		ResourceArn: c.ClusterArn,
																		Tags: next,
																	}),
															},
														]
													: []),
												...(removed.length
													? [
															{
																label: "UntagResource",
																run: () =>
																	kafkaCall(scope, "UntagResource", {
																		ResourceArn: c.ClusterArn,
																		TagKeys: removed,
																	}),
															},
														]
													: []),
											]}
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
			{!createRegions.includes(scope.region) && (
				<Alert>
					Cluster creation is supported in us-east-1, us-west-1 and us-west-2.
				</Alert>
			)}
			{(dialog === "create" || (dialog === "monitor" && !!c)) && (
				<ClusterDialog
					scope={scope}
					docker={docker}
					cluster={dialog === "create" ? undefined : c}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
			{dialog === "delete" && !!c && (
				<ActionDialog
					title="Delete cluster"
					submit="Delete cluster"
					confirmation={c.ClusterName}
					onClose={() => setDialog("")}
					onSaved={() => {
						saved();
						navigate({ href: kafkaHref(scope) });
					}}
					plan={async () => [
						{
							label: "DeleteCluster",
							run: () =>
								kafkaCall(scope, "DeleteCluster", {
									ClusterArn: c.ClusterArn,
									CurrentVersion: c.CurrentVersion,
								}),
						},
					]}
				>
					<Alert type="warning">
						Delete {c.ClusterName} permanently, including its broker container
						and data volume in Docker mode.
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
