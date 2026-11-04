import {
	Alert,
	Box,
	Button,
	Container,
	ContentLayout,
	FormField,
	Header,
	KeyValuePairs,
	Modal,
	Select,
	SpaceBetween,
	StatusIndicator,
	Table,
	Tabs,
	Textarea,
} from "@cloudscape-design/components";
import { useEffect, useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { ConsoleTags } from "#/components/ConsoleTags";
import type { Resource } from "#/lib/api";
import {
	alarmInput,
	describeAlarm,
	type HistoryItem,
	type MetricQuery,
	states,
	timestamp,
	type WatchScope,
	watchCall,
	watchTagPlan,
} from "#/lib/cloudwatch";
import { errorText } from "#/lib/consoleActions";
import { usePoll } from "#/lib/usePoll";
import { AlarmForm } from "./AlarmForm";
import { MetricGraph } from "./MetricGraph";

export function AlarmState({ value }: { value?: string }) {
	return (
		<StatusIndicator
			type={
				value === "ALARM" ? "error" : value === "OK" ? "success" : "warning"
			}
		>
			{value ?? "INSUFFICIENT_DATA"}
		</StatusIndicator>
	);
}
export function AlarmDetails({
	scope,
	resource,
	enabled,
	onChanged,
	onDeleted,
}: {
	scope: WatchScope;
	resource?: Resource;
	enabled: boolean;
	onChanged: (message: string) => void;
	onDeleted: () => void;
}) {
	const poll = usePoll(async () =>
		enabled
			? {
					alarm: await describeAlarm(scope),
					tags: await watchCall<{ Tags?: { Key: string; Value: string }[] }>(
						scope,
						"ListTagsForResource",
						{
							ResourceARN: `arn:aws:cloudwatch:${scope.region}:${scope.account}:alarm:${scope.alarm}`,
						},
					),
				}
			: undefined,
	);
	const [dialog, setDialog] = useState(""),
		[state, setState] = useState("ALARM"),
		[reason, setReason] = useState("Testing alarm actions from the console.");
	const alarm = poll.data?.alarm,
		tags = Object.fromEntries(
			(poll.data?.tags.Tags ?? []).map((t) => [t.Key, t.Value]),
		);
	const saved = (message: string) => {
		setDialog("");
		poll.reload();
		onChanged(message);
	};
	const queries: MetricQuery[] =
		alarm?.Metrics ??
		(alarm?.Namespace && alarm?.MetricName && alarm?.Period
			? [
					{
						Id: "m0",
						MetricStat: {
							Metric: {
								Namespace: alarm.Namespace,
								MetricName: alarm.MetricName,
								Dimensions: alarm.Dimensions,
							},
							Period: alarm.Period,
							Stat: alarm.ExtendedStatistic ?? alarm.Statistic ?? "Average",
							...(alarm.Unit ? { Unit: alarm.Unit } : {}),
						},
					},
				]
			: []);
	const graphAvailable = queries.every((q) => q.MetricStat?.Period !== 20);
	return (
		<ContentLayout
			header={
				<Header
					variant="h1"
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button
								iconName="refresh"
								ariaLabel="Refresh alarm"
								onClick={poll.reload}
							/>
							<Button
								disabled={!enabled || !alarm}
								onClick={() => setDialog("edit")}
							>
								Edit
							</Button>
							<Button
								disabled={!enabled || !alarm}
								onClick={() => setDialog("actions")}
							>
								{alarm?.ActionsEnabled ? "Disable" : "Enable"} actions
							</Button>
							<Button
								disabled={!enabled || !alarm}
								onClick={() => setDialog("state")}
							>
								Set state
							</Button>
							<Button
								disabled={!enabled || !alarm}
								onClick={() => setDialog("delete")}
							>
								Delete
							</Button>
						</SpaceBetween>
					}
				>
					{scope.alarm}
				</Header>
			}
		>
			<SpaceBetween size="l">
				{!!poll.error && <Alert type="error">{poll.error}</Alert>}
				<Container header={<Header variant="h2">Alarm overview</Header>}>
					<KeyValuePairs
						columns={3}
						items={[
							{
								label: "State",
								value: (
									<AlarmState
										value={
											alarm?.StateValue ??
											(resource?.details?.State as string | undefined)
										}
									/>
								),
							},
							{
								label: "Type",
								value: alarm?.Type ?? String(resource?.details?.Type ?? "—"),
							},
							{
								label: "Actions",
								value: alarm
									? alarm.ActionsEnabled
										? "Enabled"
										: "Disabled"
									: "—",
							},
							{ label: "ARN", value: alarm?.AlarmArn ?? resource?.arn ?? "—" },
							{ label: "Description", value: alarm?.AlarmDescription || "—" },
							{
								label: "Updated",
								value: timestamp(alarm?.StateUpdatedTimestamp),
							},
							{
								label: "State reason",
								value:
									alarm?.StateReason ??
									String(resource?.details?.Reason ?? "—"),
							},
						]}
					/>
				</Container>
				{alarm && (
					<Tabs
						tabs={[
							{
								id: "details",
								label: "Details",
								content: (
									<SpaceBetween size="l">
										{alarm.Type === "CompositeAlarm" ? (
											<Container
												header={<Header variant="h2">Alarm rule</Header>}
											>
												<CodeBlock text={alarm.AlarmRule ?? ""} />
											</Container>
										) : (
											<Container
												header={
													<Header variant="h2">Metric and threshold</Header>
												}
											>
												<SpaceBetween size="m">
													<KeyValuePairs
														columns={3}
														items={[
															{
																label: "Metric",
																value: alarm.Metrics
																	? "Metric math"
																	: `${alarm.Namespace}/${alarm.MetricName}`,
															},
															{
																label: "Condition",
																value: `${alarm.ComparisonOperator} ${alarm.Threshold}`,
															},
															{
																label: "Evaluation",
																value: `${alarm.DatapointsToAlarm ?? alarm.EvaluationPeriods} of ${alarm.EvaluationPeriods} data points`,
															},
															{
																label: "Missing data",
																value: alarm.TreatMissingData ?? "missing",
															},
														]}
													/>
													{graphAvailable ? (
														<MetricGraph
															key={JSON.stringify(queries)}
															scope={scope}
															queries={queries}
															enabled={enabled}
															threshold={alarm.Threshold}
															autoLoad
														/>
													) : (
														<Alert type="info">
															This alarm uses a 20-second evaluation period.
															Metric graph reads support 1, 5, 10, 30, or
															multiples of 60 seconds.
														</Alert>
													)}
												</SpaceBetween>
											</Container>
										)}
										<Container header={<Header variant="h2">Actions</Header>}>
											<KeyValuePairs
												columns={3}
												items={(
													[
														["ALARM", alarm.AlarmActions],
														["OK", alarm.OKActions],
														[
															"INSUFFICIENT_DATA",
															alarm.InsufficientDataActions,
														],
													] as [string, string[] | undefined][]
												).map(([label, actions]) => ({
													label,
													value: actions?.length
														? actions.map((a) => <Box key={a}>{a}</Box>)
														: "None",
												}))}
											/>
										</Container>
									</SpaceBetween>
								),
							},
							{
								id: "history",
								label: "History",
								content: <AlarmHistory scope={scope} enabled={enabled} />,
							},
							{
								id: "tags",
								label: "Tags",
								content: (
									<ConsoleTags
										tags={tags}
										enabled={enabled}
										plan={(next, removed) =>
											watchTagPlan(scope, alarm.AlarmArn ?? "", next, removed)
										}
										onChanged={() => saved("Alarm tags updated.")}
									/>
								),
							},
							{
								id: "configuration",
								label: "Configuration",
								content: (
									<CodeBlock
										text={JSON.stringify(alarmInput(alarm), null, 2)}
									/>
								),
							},
						]}
					/>
				)}
				{dialog === "edit" && alarm && (
					<AlarmForm
						scope={scope}
						initial={alarm}
						onClose={() => setDialog("")}
						onSaved={() => saved("Alarm updated.")}
					/>
				)}
				{dialog === "actions" && alarm && (
					<ActionDialog
						title={`${alarm.ActionsEnabled ? "Disable" : "Enable"} alarm actions`}
						submit={alarm.ActionsEnabled ? "Disable actions" : "Enable actions"}
						onClose={() => setDialog("")}
						onSaved={() => saved("Alarm actions updated.")}
						plan={async () => [
							{
								label: "Update actions",
								run: () =>
									watchCall(
										scope,
										alarm.ActionsEnabled
											? "DisableAlarmActions"
											: "EnableAlarmActions",
										{ AlarmNames: [scope.alarm] },
									),
							},
						]}
					>
						<Alert type="info">
							{alarm.ActionsEnabled
								? "Alarm evaluation continues while actions are disabled."
								: "Future alarm transitions can invoke the configured actions."}
						</Alert>
					</ActionDialog>
				)}
				{dialog === "delete" && (
					<ActionDialog
						title="Delete alarm"
						submit="Delete alarm"
						confirmation={scope.alarm}
						onClose={() => setDialog("")}
						onSaved={() => {
							saved("Alarm deleted.");
							onDeleted();
						}}
						plan={async () => [
							{
								label: "Delete alarm",
								run: () =>
									watchCall(scope, "DeleteAlarms", {
										AlarmNames: [scope.alarm],
									}),
							},
						]}
					>
						<Box>Delete this alarm and stop its evaluation.</Box>
					</ActionDialog>
				)}
				{dialog === "state" && (
					<ActionDialog
						title="Set alarm state"
						submit="Set state"
						onClose={() => setDialog("")}
						onSaved={() => saved("Alarm state set.")}
						plan={async () => {
							if (!reason.trim())
								throw new Error("Enter a reason for this state change.");
							return [
								{
									label: "Set alarm state",
									run: () =>
										watchCall(scope, "SetAlarmState", {
											AlarmName: scope.alarm,
											StateValue: state,
											StateReason: reason,
										}),
								},
							];
						}}
					>
						<SpaceBetween size="l">
							<Alert type="warning">
								Changing state can invoke enabled alarm actions and publish a
								state-change event. Metric alarms resume evaluation after one
								period; composite alarms resume when their computed state
								changes.
							</Alert>
							<FormField label="State">
								<Select
									selectedOption={{ value: state, label: state }}
									options={states.map((value) => ({ value, label: value }))}
									onChange={({ detail }) =>
										setState(detail.selectedOption.value ?? "ALARM")
									}
								/>
							</FormField>
							<FormField label="Reason">
								<Textarea
									ariaLabel="Alarm state reason"
									value={reason}
									onChange={({ detail }) => setReason(detail.value)}
								/>
							</FormField>
						</SpaceBetween>
					</ActionDialog>
				)}
			</SpaceBetween>
		</ContentLayout>
	);
}
function AlarmHistory({
	scope,
	enabled,
}: {
	scope: WatchScope;
	enabled: boolean;
}) {
	const [kind, setKind] = useState("");
	return (
		<SpaceBetween size="m">
			<Select
				selectedOption={{ value: kind, label: kind || "All history" }}
				options={["", "ConfigurationUpdate", "StateUpdate", "Action"].map(
					(value) => ({ value, label: value || "All history" }),
				)}
				onChange={({ detail }) => setKind(detail.selectedOption.value ?? "")}
			/>
			<HistoryPages key={kind} scope={scope} enabled={enabled} kind={kind} />
		</SpaceBetween>
	);
}
function HistoryPages({
	scope,
	enabled,
	kind,
}: {
	scope: WatchScope;
	enabled: boolean;
	kind: string;
}) {
	const [items, setItems] = useState<HistoryItem[]>([]),
		[token, setToken] = useState<string>(),
		[busy, setBusy] = useState(false),
		[error, setError] = useState(""),
		[detail, setDetail] = useState<HistoryItem>();
	const generation = useRef(0),
		locked = useRef(false),
		seen = useRef(new Set<string>());
	const load = async (next?: string) => {
		if (!enabled || locked.current) return;
		locked.current = true;
		const id = ++generation.current;
		setBusy(true);
		setError("");
		try {
			const out = await watchCall<{
				AlarmHistoryItems?: HistoryItem[];
				NextToken?: string;
			}>(scope, "DescribeAlarmHistory", {
				AlarmName: scope.alarm,
				ScanBy: "TimestampDescending",
				MaxRecords: 50,
				...(kind ? { HistoryItemType: kind } : {}),
				...(next ? { NextToken: next } : {}),
			});
			if (id !== generation.current) return;
			if (out.NextToken && seen.current.has(out.NextToken))
				throw new Error(
					"History pagination did not advance. Refresh to retry.",
				);
			if (!next) seen.current.clear();
			if (out.NextToken) seen.current.add(out.NextToken);
			setItems((old) =>
				next
					? [...old, ...(out.AlarmHistoryItems ?? [])]
					: (out.AlarmHistoryItems ?? []),
			);
			setToken(out.NextToken);
		} catch (e) {
			if (id === generation.current) setError(errorText(e));
		} finally {
			if (id === generation.current) {
				locked.current = false;
				setBusy(false);
			}
		}
	};
	// biome-ignore lint/correctness/useExhaustiveDependencies: keyed by scope and history type
	useEffect(() => {
		locked.current = false;
		void load();
		return () => {
			generation.current++;
			locked.current = false;
		};
	}, []);
	return (
		<SpaceBetween size="m">
			{!!error && <Alert type="error">{error}</Alert>}
			<Table
				items={items}
				loading={busy}
				header={
					<Header
						actions={
							<Button
								disabled={!enabled || busy}
								onClick={() => {
									seen.current.clear();
									void load();
								}}
							>
								Refresh history
							</Button>
						}
					>
						Alarm history
					</Header>
				}
				wrapLines
				columnDefinitions={[
					{ id: "time", header: "Time", cell: (r) => timestamp(r.Timestamp) },
					{ id: "type", header: "Type", cell: (r) => r.HistoryItemType ?? "—" },
					{
						id: "summary",
						header: "Summary",
						cell: (r) => r.HistorySummary ?? "—",
					},
					{
						id: "detail",
						header: "Details",
						cell: (r) => (
							<Button variant="inline-link" onClick={() => setDetail(r)}>
								View details
							</Button>
						),
					},
				]}
				empty={<Box>No matching alarm history.</Box>}
			/>
			{!!token && (
				<Button disabled={busy} onClick={() => void load(token)}>
					Load older history
				</Button>
			)}
			{detail && (
				<Modal
					visible
					header="Alarm history details"
					onDismiss={() => setDetail(undefined)}
					closeAriaLabel="Close history details"
				>
					<CodeBlock text={JSON.stringify(detail, null, 2)} />
				</Modal>
			)}
		</SpaceBetween>
	);
}
