import {
	Alert,
	Box,
	Button,
	Container,
	ContentLayout,
	FormField,
	Header,
	Input,
	SpaceBetween,
	Tabs,
	Textarea,
} from "@cloudscape-design/components";
import { type ReactNode, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { ConsoleTags } from "#/components/ConsoleTags";
import {
	type DashboardWidget,
	dashboardQueries,
	type WatchScope,
	watchCall,
	watchTagPlan,
} from "#/lib/cloudwatch";
import { errorText, jsonObject, stringMap } from "#/lib/consoleActions";
import { usePoll } from "#/lib/usePoll";
import { MetricGraph } from "./MetricGraph";

export function DashboardForm({
	scope,
	initial,
	onClose,
	onSaved,
}: {
	scope: WatchScope;
	initial?: { name: string; body: string };
	onClose: () => void;
	onSaved: (name: string) => void;
}) {
	const [name, setName] = useState(initial?.name ?? ""),
		[body, setBody] = useState(initial?.body ?? '{\n  "widgets": []\n}'),
		[tags, setTags] = useState("{}");
	return (
		<ActionDialog
			title={initial ? "Edit dashboard" : "Create dashboard"}
			submit={initial ? "Save changes" : "Create dashboard"}
			onClose={onClose}
			onSaved={() => onSaved(name)}
			plan={async () => {
				if (!/^[A-Za-z0-9_-]{1,255}$/.test(name))
					throw new Error(
						"Dashboard names use 1–255 letters, numbers, hyphens, or underscores.",
					);
				const doc = jsonObject(body, "Dashboard body");
				if (!Array.isArray(doc.widgets))
					throw new Error("Dashboard body must include a widgets array.");
				if (new TextEncoder().encode(body).length > 1048576)
					throw new Error("Dashboard body must use at most 1 MiB.");
				const next = initial ? undefined : stringMap(tags, "Tags");
				if (!initial) {
					const out = await watchCall<{
						DashboardEntries?: { DashboardName: string }[];
					}>(scope, "ListDashboards", { DashboardNamePrefix: name });
					if (out.DashboardEntries?.some((d) => d.DashboardName === name))
						throw new Error(
							"A dashboard with this name already exists. Open it to edit.",
						);
				}
				return [
					{
						label: "Save dashboard",
						run: () =>
							watchCall(scope, "PutDashboard", {
								DashboardName: name,
								DashboardBody: body,
							}),
					},
					...(!initial && next && Object.keys(next).length
						? watchTagPlan(
								scope,
								`arn:aws:cloudwatch::${scope.account}:dashboard/${name}`,
								next,
								[],
							)
						: []),
				];
			}}
		>
			<SpaceBetween size="l">
				<FormField label="Dashboard name">
					<Input
						ariaLabel="Dashboard name"
						value={name}
						disabled={!!initial}
						onChange={({ detail }) => setName(detail.value)}
					/>
				</FormField>
				<FormField
					label="Dashboard body (JSON)"
					description="Use an AWS-style widgets array. Metric and text widgets have previews; other widgets are preserved in source."
				>
					<Textarea
						ariaLabel="Dashboard body"
						rows={18}
						value={body}
						onChange={({ detail }) => setBody(detail.value)}
					/>
				</FormField>
				{!initial && (
					<FormField label="Tags (JSON) — optional">
						<Textarea
							ariaLabel="Dashboard tags"
							value={tags}
							onChange={({ detail }) => setTags(detail.value)}
						/>
					</FormField>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}
export function DashboardDetails({
	scope,
	enabled,
	onChanged,
	onDeleted,
}: {
	scope: WatchScope;
	enabled: boolean;
	onChanged: (message: string) => void;
	onDeleted: () => void;
}) {
	const poll = usePoll(
		async () =>
			enabled
				? {
						dashboard: await watchCall<{
							DashboardName: string;
							DashboardBody: string;
							DashboardArn: string;
						}>(scope, "GetDashboard", { DashboardName: scope.dashboard }),
						tags: await watchCall<{ Tags?: { Key: string; Value: string }[] }>(
							scope,
							"ListTagsForResource",
							{
								ResourceARN: `arn:aws:cloudwatch::${scope.account}:dashboard/${scope.dashboard}`,
							},
						),
					}
				: undefined,
		0,
	);
	const [dialog, setDialog] = useState("");
	const board = poll.data?.dashboard,
		tags = Object.fromEntries(
			(poll.data?.tags.Tags ?? []).map((t) => [t.Key, t.Value]),
		);
	let widgets: DashboardWidget[] = [];
	if (board) {
		try {
			widgets = (JSON.parse(board.DashboardBody).widgets ??
				[]) as DashboardWidget[];
		} catch {
			/* The API validates JSON on write. Source remains visible for diagnosis. */
		}
	}
	return (
		<ContentLayout
			header={
				<Header
					variant="h1"
					description="Dashboards are stored in the selected account and Region."
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button
								iconName="refresh"
								ariaLabel="Refresh dashboard"
								disabled={!enabled}
								onClick={poll.reload}
							/>
							<Button
								disabled={!enabled || !board}
								onClick={() => setDialog("edit")}
							>
								Edit dashboard
							</Button>
							<Button
								disabled={!enabled || !board}
								onClick={() => setDialog("delete")}
							>
								Delete
							</Button>
						</SpaceBetween>
					}
				>
					{scope.dashboard}
				</Header>
			}
		>
			<SpaceBetween size="l">
				{poll.error && <Alert type="error">{poll.error}</Alert>}
				{!enabled && (
					<Alert type="info">
						Use signed CLI/SDK requests to read or manage dashboard bodies.
					</Alert>
				)}
				{board && (
					<Tabs
						tabs={[
							{
								id: "preview",
								label: "Dashboard",
								content: (
									<SpaceBetween size="l">
										<Box color="text-body-secondary">
											Metric previews use the widget's statistic, period,
											dimensions, and Region. Text is displayed as plain text.
											Layout and other widget types remain in the source.
										</Box>
										{!widgets.length && (
											<Box padding="l" textAlign="center">
												This dashboard has no widgets. Edit the dashboard to add
												metrics or text.
											</Box>
										)}
										{widgets.map((widget, i) => (
											<DashboardPreview
												// biome-ignore lint/suspicious/noArrayIndexKey: immutable source snapshot; definition changes remount previews
												key={`${i}/${JSON.stringify(widget)}`}
												widget={widget}
												scope={scope}
												enabled={enabled}
												index={i}
											/>
										))}
									</SpaceBetween>
								),
							},
							{
								id: "source",
								label: "Source",
								content: <CodeBlock text={board.DashboardBody} />,
							},
							{
								id: "tags",
								label: "Tags",
								content: (
									<ConsoleTags
										tags={tags}
										enabled={enabled}
										plan={(next, removed) =>
											watchTagPlan(scope, board.DashboardArn, next, removed)
										}
										onChanged={() => {
											poll.reload();
											onChanged("Dashboard tags updated.");
										}}
									/>
								),
							},
						]}
					/>
				)}
				{dialog === "edit" && board && (
					<DashboardForm
						scope={scope}
						initial={{ name: board.DashboardName, body: board.DashboardBody }}
						onClose={() => setDialog("")}
						onSaved={() => {
							setDialog("");
							poll.reload();
							onChanged("Dashboard updated.");
						}}
					/>
				)}
				{dialog === "delete" && (
					<ActionDialog
						title="Delete dashboard"
						submit="Delete dashboard"
						confirmation={scope.dashboard}
						onClose={() => setDialog("")}
						onSaved={() => {
							setDialog("");
							onChanged("Dashboard deleted.");
							onDeleted();
						}}
						plan={async () => [
							{
								label: "Delete dashboard",
								run: () =>
									watchCall(scope, "DeleteDashboards", {
										DashboardNames: [scope.dashboard],
									}),
							},
						]}
					>
						<Box>Delete this dashboard's saved definition.</Box>
					</ActionDialog>
				)}
			</SpaceBetween>
		</ContentLayout>
	);
}
function DashboardPreview({
	widget,
	scope,
	enabled,
	index,
}: {
	widget: DashboardWidget;
	scope: WatchScope;
	enabled: boolean;
	index: number;
}) {
	const properties =
		widget && typeof widget === "object" ? (widget.properties ?? {}) : {};
	const title = String(properties.title ?? `Widget ${index + 1}`);
	let content: ReactNode;
	if (widget?.type === "text")
		content = (
			<div style={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}>
				{String(properties.markdown ?? "")}
			</div>
		);
	else if (widget?.type === "metric") {
		try {
			const request = dashboardQueries(properties, scope.region);
			content = (
				<SpaceBetween size="m">
					<Box color="text-body-secondary">{request.region}</Box>
					<MetricGraph
						scope={{ ...scope, region: request.region }}
						queries={request.queries}
						enabled={enabled}
						autoLoad
					/>
				</SpaceBetween>
			);
		} catch (err) {
			content = (
				<Alert type="info">
					{errorText(err)} The widget definition remains available in Source.
				</Alert>
			);
		}
	} else
		content = (
			<Alert type="info">
				A preview for {String(widget?.type ?? "this widget")} is unavailable.
				The widget definition remains available in Source.
			</Alert>
		);
	return (
		<Container header={<Header variant="h2">{title}</Header>}>
			{content}
		</Container>
	);
}
