import { Alert, Link, SpaceBetween, Tabs } from "@cloudscape-design/components";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { ConsoleLayout } from "#/components/Console";
import { ConsoleTags } from "#/components/ConsoleTags";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { BrokerDialog } from "#/components/mq/BrokerDialog";
import { Configurations } from "#/components/mq/Configurations";
import { Details } from "#/components/mq/Details";
import { Users } from "#/components/mq/Users";
import { isoTime } from "#/lib/format";
import { mqCall, mqList } from "#/lib/mq";
import { type Broker, type MQScope, mqHref, pending } from "#/lib/mqHelpers";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { tagList, tagMap } from "#/lib/tags";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
export const Route = createFileRoute("/services/mq")({
	validateSearch: (s: Record<string, unknown>) => ({
		account: String(s.account ?? ""),
		region: String(s.region ?? ""),
		broker: String(s.broker ?? ""),
		tab: ["tags", "users", "configurations"].includes(String(s.tab))
			? String(s.tab)
			: "overview",
	}),
	component: MQPage,
});
function MQPage() {
	const { account, region, canWrite, capabilities, inventory } = useScope();
	const search = Route.useSearch(),
		scope = { account, region },
		home = mqHref(scope);
	const enabled =
		!!capabilities.data?.pages.mq &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const docker = capabilities.data?.mq_runtime === "docker";
	return (
		<ConsoleLayout
			contentType={search.broker ? "default" : "table"}
			navigationHeader={{ text: "Amazon MQ", href: home }}
			activeHref={home}
			navigationItems={[
				{ type: "link", text: "Brokers", href: home },
				{
					type: "link",
					text: "Configurations",
					href: mqHref(scope, "", "configurations"),
				},
				{ type: "link", text: "All services", href: "/services" },
			]}
			breadcrumbs={[
				{ text: "Amazon MQ", href: home },
				...(search.broker
					? [{ text: search.broker, href: mqHref(scope, search.broker) }]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{!!(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.mq && (
						<Alert type="warning">
							Amazon MQ is not enabled on this server.
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
							? "Each broker runs one local ActiveMQ or RabbitMQ container. Use the returned loopback endpoints and web console URL once RUNNING. Deployment mode and instance type are metadata; no HA topology is created."
							: "Brokers are stored as records. No messaging engines run and no endpoints or web console are available. Set mq.brokers to docker for working brokers."}
					</Alert>
					<Workspace
						key={`${account}/${region}/${enabled}/${search.broker}`}
						scope={scope}
						enabled={enabled}
						docker={docker}
						name={search.broker}
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
	scope: MQScope;
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
			const summaries = enabled
				? await mqList<Broker>(scope, "ListBrokers", "BrokerSummaries")
				: [];
			const rows = await Promise.all(
				summaries.map((b) =>
					mqCall<Broker>(scope, "DescribeBroker", { BrokerId: b.BrokerId }),
				),
			);

			setChanging(rows.some((c) => pending(c.BrokerState)));
			return rows;
		},
		changing ? 2000 : 0,
	);
	const c = poll.data?.find((c) => c.BrokerId === name);
	const saved = () => {
		setDialog("");
		setChanging(true);
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{!name && tab === "configurations" ? (
				<Configurations scope={scope} docker={docker} enabled={enabled} />
			) : !name ? (
				<Rows
					title="Brokers"
					items={poll.data ?? []}
					loading={poll.loading}
					emptyText="Choose Create broker to add one."
					actions={
						<Buttons>
							<Action onClick={poll.reload}>Refresh</Action>
							<Action disabled={!enabled} onClick={() => setDialog("create")}>
								Create broker
							</Action>
						</Buttons>
					}
					columns={[
						{
							id: "name",
							header: "Name",
							cell: (c) => (
								<Link href={mqHref(scope, c.BrokerId)} onFollow={follow}>
									{c.BrokerName}
								</Link>
							),
						},
						{ id: "type", header: "Type", cell: (c) => c.EngineType },
						{
							id: "version",
							header: "Engine version",
							cell: (c) => c.EngineVersion ?? "-",
						},
						{ id: "state", header: "State", cell: (c) => c.BrokerState ?? "-" },
						{
							id: "count",
							header: "Deployment mode",
							cell: (c) => c.DeploymentMode ?? "-",
						},
						{
							id: "created",
							header: "Created",
							cell: (c) => isoTime(c.Created),
						},
					]}
				/>
			) : c ? (
				<SpaceBetween size="m">
					<Buttons>
						<Action onClick={() => navigate({ href: mqHref(scope) })}>
							Brokers
						</Action>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action
							disabled={!enabled || pending(c.BrokerState)}
							onClick={() => setDialog("edit")}
						>
							Edit broker
						</Action>
						<Action
							disabled={!enabled || (docker && c.BrokerState !== "RUNNING")}
							onClick={() => setDialog("reboot")}
						>
							Reboot broker
						</Action>
						<Action
							disabled={!enabled || pending(c.BrokerState)}
							onClick={() => setDialog("delete")}
						>
							Delete broker
						</Action>
					</Buttons>
					<Tabs
						activeTabId={tab}
						onChange={({ detail }) =>
							navigate({ href: mqHref(scope, name, detail.activeTabId) })
						}
						tabs={[
							{
								id: "users",
								label: "Users",
								content:
									tab === "users" ? (
										<Users
											scope={scope}
											broker={c}
											docker={docker}
											enabled={enabled}
										/>
									) : null,
							},
							{
								id: "configurations",
								label: "Configurations",
								content:
									tab === "configurations" ? (
										<Configurations
											scope={scope}
											docker={docker}
											enabled={enabled}
										/>
									) : null,
							},
							{
								id: "overview",
								label: "Overview",
								content:
									tab === "overview" ? (
										<Details
											key={`${c.BrokerArn}/${c.BrokerState}/${c.EngineVersion}`}
											broker={c}
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
											enabled={enabled && !pending(c.BrokerState)}
											onChanged={saved}
											plan={(next, removed) => [
												...(Object.keys(next).length
													? [
															{
																label: "CreateTags",
																run: () =>
																	mqCall(scope, "CreateTags", {
																		ResourceArn: c.BrokerArn,
																		Tags: next,
																	}),
															},
														]
													: []),
												...(removed.length
													? [
															{
																label: "DeleteTags",
																run: () =>
																	mqCall(scope, "DeleteTags", {
																		ResourceArn: c.BrokerArn,
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
					<Alert>Broker not found in this account and Region.</Alert>
				)
			)}
			{(dialog === "create" || (dialog === "edit" && !!c)) && (
				<BrokerDialog
					scope={scope}
					docker={docker}
					broker={dialog === "create" ? undefined : c}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
			{dialog === "reboot" && !!c && (
				<ActionDialog
					title="Reboot broker"
					submit="Reboot broker"
					onClose={() => setDialog("")}
					onSaved={saved}
					plan={async () => [
						{
							label: "RebootBroker",
							run: () =>
								mqCall(scope, "RebootBroker", { BrokerId: c.BrokerId }),
						},
					]}
				>
					<Alert>
						Reboot {c.BrokerName}. Connections will be interrupted in Docker
						mode and pending engine updates applied.
					</Alert>
				</ActionDialog>
			)}
			{dialog === "delete" && !!c && (
				<ActionDialog
					title="Delete broker"
					submit="Delete broker"
					confirmation={c.BrokerName}
					onClose={() => setDialog("")}
					onSaved={() => {
						saved();
						navigate({ href: mqHref(scope) });
					}}
					plan={async () => [
						{
							label: "DeleteBroker",
							run: () =>
								mqCall(scope, "DeleteBroker", {
									BrokerId: c.BrokerId,
								}),
						},
					]}
				>
					<Alert type="warning">
						Delete {c.BrokerName} permanently, including its broker container
						and data volume in Docker mode.
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
