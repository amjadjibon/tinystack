import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	ContentLayout,
	Flashbar,
	Header,
	Link,
	Pagination,
	Select,
	SpaceBetween,
	Table,
	TextFilter,
} from "@cloudscape-design/components";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { ConsoleLayout } from "#/components/Console";
import { AlarmDetails, AlarmState } from "#/components/cloudwatch/AlarmDetails";
import { AlarmForm } from "#/components/cloudwatch/AlarmForm";
import {
	DashboardDetails,
	DashboardForm,
} from "#/components/cloudwatch/DashboardDetails";
import { Metrics } from "#/components/cloudwatch/Metrics";
import { cloudwatchHref, states, type WatchScope } from "#/lib/cloudwatch";
import { logsHref } from "#/lib/logs";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { useFollow } from "#/lib/useFollow";
export const Route = createFileRoute("/services/cloudwatch")({
	validateSearch: (s: Record<string, unknown>) => ({
		account: String(s.account ?? ""),
		region: String(s.region ?? ""),
		alarm: String(s.alarm ?? ""),
		dashboard: String(s.dashboard ?? ""),
		section:
			s.section === "metrics"
				? "metrics"
				: s.section === "dashboards"
					? "dashboards"
					: "alarms",
	}),
	component: CloudWatchPage,
});
function CloudWatchPage() {
	const search = Route.useSearch(),
		context = useScope(),
		[notice, setNotice] = useState({ scope: "", message: "" });
	const scopeId = `${context.account}/${context.region}`;
	const enabled =
		!!context.capabilities.data?.pages.cloudwatch &&
		context.canWrite &&
		/^\d{12}$/.test(context.account) &&
		validRegion(context.region);
	return (
		<WatchWorkspace
			key={`${scopeId}/${search.section}/${search.alarm}/${search.dashboard}/${enabled}`}
			scope={{
				account: context.account,
				region: context.region,
				alarm: search.alarm,
				dashboard: search.dashboard,
			}}
			section={search.section}
			enabled={enabled}
			notice={notice.scope === scopeId ? notice.message : ""}
			setNotice={(message) => setNotice({ scope: scopeId, message })}
		/>
	);
}
function WatchWorkspace({
	scope,
	section,
	enabled,
	notice,
	setNotice,
}: {
	scope: WatchScope;
	section: string;
	enabled: boolean;
	notice: string;
	setNotice: (m: string) => void;
}) {
	const navigate = useNavigate(),
		follow = useFollow(),
		{ canWrite, inventory, capabilities } = useScope(),
		mounted = useRef(true);
	useEffect(() => {
		mounted.current = true;
		return () => {
			mounted.current = false;
		};
	}, []);
	const [creating, setCreating] = useState(false),
		[state, setState] = useState(""),
		[kind, setKind] = useState("");
	const href = (next: string, alarm = "", dashboard = "") =>
		cloudwatchHref({ ...scope, alarm, dashboard }, next);
	const go = (next: string, alarm = "", dashboard = "") => {
		if (mounted.current) navigate({ href: href(next, alarm, dashboard) });
	};
	const changed = (message: string) => {
		inventory.reload();
		if (mounted.current) setNotice(message);
	};
	const rows = (inventory.data ?? []).filter(
		(r) =>
			r.service === "cloudwatch" &&
			r.account === scope.account &&
			r.region === scope.region &&
			r.type === (section === "dashboards" ? "dashboard" : "alarm") &&
			(!state || r.details?.State === state) &&
			(!kind || r.details?.Type === kind),
	);
	const collection = useCollection(rows, {
		filtering: {},
		pagination: { pageSize: 20 },
		sorting: {},
	});
	const title =
			section === "metrics"
				? "Metrics"
				: section === "dashboards"
					? "Dashboards"
					: "Alarms",
		selected =
			section === "alarms"
				? scope.alarm
				: section === "dashboards"
					? scope.dashboard
					: "";
	const links = ["alarms", "metrics", "dashboards"].map((s) => ({
		type: "link" as const,
		text: s[0].toUpperCase() + s.slice(1),
		href: href(s),
	}));
	return (
		<ConsoleLayout
			navigationHeader={{ text: "CloudWatch", href: href("alarms") }}
			activeHref={href(section)}
			navigationItems={[
				...links,
				{ type: "divider" },
				{ type: "link", text: "Logs", href: logsHref(scope) },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "CloudWatch", href: href("alarms") },
				{ text: title, href: href(section) },
				...(selected
					? [{ text: selected, href: cloudwatchHref(scope, section) }]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{!!notice && (
						<Flashbar
							items={[
								{
									id: "watch-success",
									type: "success",
									content: notice,
									dismissible: true,
									onDismiss: () => setNotice(""),
								},
							]}
						/>
					)}
					{!!(inventory.error || capabilities.error) && (
						<Alert type="error">{inventory.error || capabilities.error}</Alert>
					)}
					{capabilities.data?.requires_signature && !canWrite && (
						<Alert type="info">
							Connect a root session for the selected account using Console
							session in the top bar, or use signed AWS CLI/SDK requests.
						</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.cloudwatch && (
						<Alert type="warning">
							CloudWatch is not enabled on this server.
						</Alert>
					)}
					{section === "metrics" ? (
						<ContentLayout header={<Header variant="h1">Metrics</Header>}>
							<Metrics
								scope={scope}
								enabled={enabled}
								onChanged={changed}
								onAlarmCreated={(name) => {
									changed("Alarm created.");
									go("alarms", name);
								}}
							/>
						</ContentLayout>
					) : section === "alarms" && scope.alarm ? (
						<AlarmDetails
							scope={scope}
							resource={rows.find((r) => r.name === scope.alarm)}
							enabled={enabled}
							onChanged={changed}
							onDeleted={() => go("alarms")}
						/>
					) : section === "dashboards" && scope.dashboard ? (
						<DashboardDetails
							scope={scope}
							enabled={enabled}
							onChanged={changed}
							onDeleted={() => go("dashboards")}
						/>
					) : (
						<ContentLayout header={<Header variant="h1">{title}</Header>}>
							<Table
								{...collection.collectionProps}
								items={collection.items}
								trackBy="arn"
								wrapLines
								loading={inventory.loading}
								header={
									<Header
										counter={`(${rows.length})`}
										actions={
											<SpaceBetween direction="horizontal" size="xs">
												<Button
													iconName="refresh"
													ariaLabel={`Refresh ${title.toLowerCase()}`}
													onClick={inventory.reload}
												/>
												<Button
													variant="primary"
													disabled={!enabled}
													onClick={() => setCreating(true)}
												>
													Create{" "}
													{section === "dashboards" ? "dashboard" : "alarm"}
												</Button>
											</SpaceBetween>
										}
									>
										{title}
									</Header>
								}
								filter={
									<SpaceBetween direction="horizontal" size="xs">
										<TextFilter
											{...collection.filterProps}
											filteringPlaceholder={`Find ${title.toLowerCase()}`}
										/>
										{section === "alarms" && (
											<SpaceBetween direction="horizontal" size="xs">
												<Select
													selectedOption={{
														value: state,
														label: state || "All states",
													}}
													options={["", ...states].map((value) => ({
														value,
														label: value || "All states",
													}))}
													onChange={({ detail }) =>
														setState(detail.selectedOption.value ?? "")
													}
												/>
												<Select
													selectedOption={{
														value: kind,
														label: kind || "All alarm types",
													}}
													options={["", "MetricAlarm", "CompositeAlarm"].map(
														(value) => ({
															value,
															label: value || "All alarm types",
														}),
													)}
													onChange={({ detail }) =>
														setKind(detail.selectedOption.value ?? "")
													}
												/>
											</SpaceBetween>
										)}
									</SpaceBetween>
								}
								pagination={<Pagination {...collection.paginationProps} />}
								columnDefinitions={[
									{
										id: "name",
										header:
											section === "dashboards"
												? "Dashboard name"
												: "Alarm name",
										sortingField: "name",
										cell: (r) => (
											<Link
												href={href(
													section,
													section === "alarms" ? r.name : "",
													section === "dashboards" ? r.name : "",
												)}
												onFollow={follow}
											>
												{r.name}
											</Link>
										),
									},
									...(section === "alarms"
										? [
												{
													id: "state",
													header: "State",
													cell: (r: (typeof rows)[number]) => (
														<AlarmState
															value={r.details?.State as string | undefined}
														/>
													),
												},
												{
													id: "type",
													header: "Type",
													cell: (r: (typeof rows)[number]) =>
														String(r.details?.Type ?? "—"),
												},
												{
													id: "reason",
													header: "State reason",
													cell: (r: (typeof rows)[number]) =>
														String(r.details?.Reason ?? "—"),
												},
											]
										: [
												{
													id: "size",
													header: "Definition size",
													cell: (r: (typeof rows)[number]) =>
														`${r.details?.Size ?? 0} bytes`,
												},
											]),
								]}
								empty={
									<Box textAlign="center" padding="l">
										No matching {title.toLowerCase()}. Create{" "}
										{section === "dashboards"
											? "a dashboard to save metric widgets"
											: "an alarm to monitor metric values"}
										.
									</Box>
								}
							/>
						</ContentLayout>
					)}
					{creating && section === "alarms" && (
						<AlarmForm
							scope={scope}
							onClose={() => setCreating(false)}
							onSaved={(name) => {
								setCreating(false);
								changed("Alarm created.");
								go("alarms", name);
							}}
						/>
					)}
					{creating && section === "dashboards" && (
						<DashboardForm
							scope={scope}
							onClose={() => setCreating(false)}
							onSaved={(name) => {
								setCreating(false);
								changed("Dashboard created.");
								go("dashboards", "", name);
							}}
						/>
					)}
				</SpaceBetween>
			}
		/>
	);
}
