import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	ContentLayout,
	Flashbar,
	FormField,
	Header,
	Input,
	Link,
	Pagination,
	Select,
	SpaceBetween,
	Table,
	Textarea,
	TextFilter,
} from "@cloudscape-design/components";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { ConsoleLayout } from "#/components/Console";
import { LogGroupDetails } from "#/components/logs/LogGroupDetails";
import { LogsInsights } from "#/components/logs/LogsInsights";
import { stringMap } from "#/lib/consoleActions";
import {
	type LogsScope,
	logGroup,
	logsCall,
	logsHref,
	retentionDays,
} from "#/lib/logs";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { useFollow } from "#/lib/useFollow";
export const Route = createFileRoute("/services/logs")({
	validateSearch: (s: Record<string, unknown>) => ({
		account: String(s.account ?? ""),
		region: String(s.region ?? ""),
		group: String(s.group ?? ""),
		stream: String(s.stream ?? ""),
		section: s.section === "insights" ? "insights" : "groups",
	}),
	component: LogsPage,
});
function LogsPage() {
	const search = Route.useSearch(),
		context = useScope();
	const [notice, setNotice] = useState({ scope: "", message: "" });
	const scopeId = `${context.account}/${context.region}`;
	const enabled =
		!!context.capabilities.data?.logs &&
		!context.capabilities.data.requires_signature &&
		/^\d{12}$/.test(context.account) &&
		validRegion(context.region);
	return (
		<LogsWorkspace
			key={`${context.account}/${context.region}/${search.group}/${search.stream}/${search.section}/${enabled}`}
			scope={{
				account: context.account,
				region: context.region,
				group: search.group,
				stream: search.stream,
			}}
			section={search.section}
			enabled={enabled}
			notice={notice.scope === scopeId ? notice.message : ""}
			setNotice={(message) => setNotice({ scope: scopeId, message })}
		/>
	);
}
function LogsWorkspace({
	scope,
	section,
	enabled,
	notice,
	setNotice,
}: {
	scope: LogsScope;
	section: string;
	enabled: boolean;
	notice: string;
	setNotice: (message: string) => void;
}) {
	const navigate = useNavigate(),
		follow = useFollow(),
		{ inventory, capabilities } = useScope();
	const mounted = useRef(true);
	useEffect(() => {
		mounted.current = true;
		return () => {
			mounted.current = false;
		};
	}, []);
	const [creating, setCreating] = useState(false),
		[name, setName] = useState(""),
		[retention, setRetention] = useState("0"),
		[klass, setKlass] = useState("STANDARD"),
		[tags, setTags] = useState("{}");
	const groups = (inventory.data ?? []).filter(
		(r) =>
			r.service === "logs" &&
			r.type === "log-group" &&
			r.account === scope.account &&
			r.region === scope.region,
	);
	const rows = groups.map((r) => ({ ...r, ...logGroup(r) })),
		collection = useCollection(rows, {
			filtering: {},
			pagination: { pageSize: 20 },
			sorting: {},
		});
	const go = (group = "", stream = "", next = "groups") => {
		if (mounted.current)
			navigate({ href: logsHref({ ...scope, group, stream }, next) });
	};
	const changed = (message: string) => {
		inventory.reload();
		if (mounted.current) setNotice(message);
	};
	const list = logsHref({ ...scope, group: "", stream: "" }),
		insights = logsHref({ ...scope, group: "", stream: "" }, "insights");
	return (
		<ConsoleLayout
			navigationHeader={{ text: "CloudWatch Logs", href: list }}
			activeHref={section === "insights" ? insights : list}
			navigationItems={[
				{ type: "link", text: "Log groups", href: list },
				{ type: "link", text: "Logs Insights", href: insights },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "CloudWatch Logs", href: list },
				{
					text: section === "insights" ? "Logs Insights" : "Log groups",
					href: section === "insights" ? insights : list,
				},
				...(scope.group
					? [{ text: scope.group, href: logsHref({ ...scope, stream: "" }) }]
					: []),
				...(scope.stream
					? [{ text: scope.stream, href: logsHref(scope) }]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{notice && (
						<Flashbar
							items={[
								{
									id: "logs-success",
									type: "success",
									content: notice,
									dismissible: true,
									onDismiss: () => setNotice(""),
								},
							]}
						/>
					)}
					{(inventory.error || capabilities.error) && (
						<Alert type="error">{inventory.error || capabilities.error}</Alert>
					)}
					{capabilities.data?.requires_signature && (
						<Alert type="info">
							Log group and stream inventory is available here. Use signed
							CLI/SDK requests to read events, run queries or manage logs.
						</Alert>
					)}
					{capabilities.data && !capabilities.data.logs && (
						<Alert type="warning">
							CloudWatch Logs is not enabled on this server.
						</Alert>
					)}
					{section === "insights" ? (
						<ContentLayout header={<Header variant="h1">Logs Insights</Header>}>
							<LogsInsights scope={scope} groups={groups} enabled={enabled} />
						</ContentLayout>
					) : scope.group ? (
						<LogGroupDetails
							scope={scope}
							resource={groups.find((g) => g.name === scope.group)}
							groups={groups}
							enabled={enabled}
							onChanged={changed}
							onDeleted={() => go()}
							onStreamDeleted={() => go(scope.group)}
						/>
					) : (
						<ContentLayout header={<Header variant="h1">Log groups</Header>}>
							<Table
								{...collection.collectionProps}
								items={collection.items}
								loading={inventory.loading}
								trackBy="arn"
								wrapLines
								header={
									<Header
										counter={`(${rows.length})`}
										description="Logs in the selected account and Region."
										actions={
											<SpaceBetween direction="horizontal" size="xs">
												<Button
													iconName="refresh"
													ariaLabel="Refresh log groups"
													onClick={inventory.reload}
												/>
												<Button
													variant="primary"
													disabled={!enabled}
													onClick={() => setCreating(true)}
												>
													Create log group
												</Button>
											</SpaceBetween>
										}
									>
										Log groups
									</Header>
								}
								filter={
									<TextFilter
										{...collection.filterProps}
										filteringPlaceholder="Find log groups"
									/>
								}
								pagination={<Pagination {...collection.paginationProps} />}
								columnDefinitions={[
									{
										id: "name",
										header: "Log group",
										sortingField: "name",
										cell: (g) => (
											<Link
												href={logsHref({ ...scope, group: g.name })}
												onFollow={follow}
											>
												{g.name}
											</Link>
										),
									},
									{
										id: "retention",
										header: "Retention",
										cell: (g) =>
											g.RetentionInDays
												? `${g.RetentionInDays} days`
												: "Never expire",
									},
									{
										id: "class",
										header: "Class",
										cell: (g) => g.LogGroupClass ?? "STANDARD",
									},
									{
										id: "bytes",
										header: "Stored bytes",
										sortingField: "StoredBytes",
										cell: (g) => g.StoredBytes ?? 0,
									},
									{
										id: "streams",
										header: "Streams",
										cell: (g) => (g.details?.Streams as unknown[])?.length ?? 0,
									},
								]}
								empty={
									<Box textAlign="center" padding="l">
										No matching log groups. Create a group or invoke a Lambda
										function to produce logs.
									</Box>
								}
							/>
						</ContentLayout>
					)}
					{creating && (
						<ActionDialog
							title="Create log group"
							submit="Create log group"
							onClose={() => setCreating(false)}
							onSaved={() => {
								setCreating(false);
								changed(`Log group ${name} created.`);
								go(name);
							}}
							plan={async () => {
								if (!/^[A-Za-z0-9_.\-/#]{1,512}$/.test(name))
									throw new Error(
										"Log group names use 1–512 letters, numbers, periods, hyphens, underscores, / or #.",
									);
								const next = stringMap(tags, "Tags");
								return [
									{
										label: "Create log group",
										run: () =>
											logsCall(scope, "CreateLogGroup", {
												LogGroupName: name,
												LogGroupClass: klass,
												Tags: next,
											}),
									},
									...(retention !== "0"
										? [
												{
													label: "Set retention",
													run: () =>
														logsCall(scope, "PutRetentionPolicy", {
															LogGroupName: name,
															RetentionInDays: Number(retention),
														}),
												},
											]
										: []),
								];
							}}
						>
							<SpaceBetween size="l">
								<FormField label="Log group name">
									<Input
										ariaLabel="Log group name"
										value={name}
										onChange={({ detail }) => setName(detail.value)}
									/>
								</FormField>
								<FormField label="Log group class">
									<Select
										selectedOption={{ value: klass, label: klass }}
										options={["STANDARD", "INFREQUENT_ACCESS"].map((value) => ({
											value,
											label: value,
										}))}
										onChange={({ detail }) =>
											setKlass(detail.selectedOption.value ?? "STANDARD")
										}
									/>
								</FormField>
								<FormField label="Retention">
									<Select
										selectedOption={{
											value: retention,
											label:
												retention === "0"
													? "Never expire"
													: `${retention} days`,
										}}
										options={[
											{ value: "0", label: "Never expire" },
											...retentionDays.map((d) => ({
												value: String(d),
												label: `${d} days`,
											})),
										]}
										onChange={({ detail }) =>
											setRetention(detail.selectedOption.value ?? "0")
										}
									/>
								</FormField>
								<FormField label="Tags (JSON) — optional">
									<Textarea
										ariaLabel="Log group tags"
										value={tags}
										onChange={({ detail }) => setTags(detail.value)}
									/>
								</FormField>
							</SpaceBetween>
						</ActionDialog>
					)}
				</SpaceBetween>
			}
		/>
	);
}
