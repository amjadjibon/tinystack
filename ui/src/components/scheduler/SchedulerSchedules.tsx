import {
	Alert,
	Button,
	ExpandableSection,
	FormField,
	KeyValuePairs,
	Modal,
	RadioGroup,
	Select,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useMemo, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Pick, Text } from "#/components/memorydb/Fields";
import { Suggest } from "#/components/scheduler/Fields";
import { errorText } from "#/lib/consoleActions";
import { unixTime } from "#/lib/format";
import {
	type ScheduleGroup,
	type SchedulerScope,
	type ScheduleSummary,
	schedulerCall,
} from "#/lib/scheduler";
import {
	type ArnResource,
	defaultGroup,
	type ExpressionKind,
	emptyScheduleForm,
	expressionHints,
	formFromSchedule,
	type RateUnit,
	type ScheduleDetail,
	type ScheduleForm,
	scheduleInput,
	targetOptions,
	targetService,
} from "#/lib/schedulerHelpers";

type Dialog = "" | "create" | "edit" | "delete";

export function SchedulerSchedules({
	scope,
	enabled,
	groups,
	schedules,
	loading,
	resources,
	onChanged,
}: {
	scope: SchedulerScope;
	enabled: boolean;
	groups: ScheduleGroup[];
	schedules: ScheduleSummary[];
	loading: boolean;
	resources: ArnResource[];
	onChanged: () => void;
}) {
	const [group, setGroup] = useState(""),
		[dialog, setDialog] = useState<Dialog>(""),
		[target, setTarget] = useState<ScheduleSummary>(),
		[form, setForm] = useState<ScheduleForm>(emptyScheduleForm()),
		[detail, setDetail] = useState<ScheduleDetail>(),
		[error, setError] = useState(""),
		[busy, setBusy] = useState("");
	const shown = group
		? schedules.filter((s) => s.GroupName === group)
		: schedules;
	const key = (s: ScheduleSummary) => `${s.GroupName}/${s.Name}`;
	const load = async (s: ScheduleSummary) => {
		setError("");
		setBusy(key(s));
		try {
			return await schedulerCall<ScheduleDetail>(scope, "GetSchedule", {
				Name: s.Name,
				GroupName: s.GroupName,
			});
		} catch (err) {
			setError(errorText(err));
		} finally {
			setBusy("");
		}
	};
	const view = async (s: ScheduleSummary) => {
		const out = await load(s);
		if (out) setDetail(out);
	};
	const edit = async (s: ScheduleSummary) => {
		const out = await load(s);
		if (!out) return;
		setTarget(s);
		setForm(formFromSchedule(out));
		setDialog("edit");
	};
	return (
		<SpaceBetween size="m">
			{error && (
				<Alert type="error" dismissible onDismiss={() => setError("")}>
					{error}
				</Alert>
			)}
			<Rows
				title="Schedules"
				emptyText="Choose Create schedule to add one."
				items={shown}
				loading={loading}
				actions={
					<Buttons>
						<GroupFilter value={group} groups={groups} onChange={setGroup} />
						<Action onClick={onChanged}>Refresh</Action>
						<Action
							disabled={!enabled}
							onClick={() => {
								setForm(emptyScheduleForm(group || defaultGroup));
								setDialog("create");
							}}
						>
							Create schedule
						</Action>
					</Buttons>
				}
				columns={[
					{ id: "name", header: "Name", cell: (s) => s.Name },
					{ id: "group", header: "Group", cell: (s) => s.GroupName },
					{ id: "state", header: "State", cell: (s) => s.State },
					{ id: "target", header: "Target", cell: (s) => s.Target?.Arn },
					{
						id: "modified",
						header: "Last modified",
						cell: (s) => unixTime(s.LastModificationDate),
					},
					{
						id: "actions",
						header: "Actions",
						cell: (s) => (
							<Buttons>
								<Button loading={busy === key(s)} onClick={() => view(s)}>
									View
								</Button>
								<Button disabled={!enabled || !!busy} onClick={() => edit(s)}>
									Edit
								</Button>
								<Action
									disabled={!enabled}
									onClick={() => {
										setTarget(s);
										setDialog("delete");
									}}
								>
									Delete
								</Action>
							</Buttons>
						),
					},
				]}
			/>
			{detail !== undefined && (
				<Modal
					visible
					size="large"
					header="Schedule definition"
					closeAriaLabel="Close dialog"
					onDismiss={() => setDetail(undefined)}
				>
					<SpaceBetween size="l">
						<KeyValuePairs
							columns={2}
							items={[
								["Name", detail.Name],
								["Group", detail.GroupName],
								["Description", detail.Description],
								["Expression", detail.ScheduleExpression],
								["Time zone", detail.ScheduleExpressionTimezone],
								["State", detail.State],
								["Flexible time window", detail.FlexibleTimeWindow?.Mode],
								[
									"Maximum window (minutes)",
									detail.FlexibleTimeWindow?.MaximumWindowInMinutes,
								],
								["Start date", unixTime(detail.StartDate)],
								["End date", unixTime(detail.EndDate)],
								["Target ARN", detail.Target?.Arn],
								["Role ARN", detail.Target?.RoleArn],
								[
									"Maximum event age (seconds)",
									detail.Target?.RetryPolicy?.MaximumEventAgeInSeconds,
								],
								[
									"Maximum retry attempts",
									detail.Target?.RetryPolicy?.MaximumRetryAttempts,
								],
								["Dead-letter queue ARN", detail.Target?.DeadLetterConfig?.Arn],
								["Action after completion", detail.ActionAfterCompletion],
								["Created", unixTime(detail.CreationDate)],
								["Modified", unixTime(detail.LastModificationDate)],
							].map(([label, value]) => ({
								label: String(label),
								value: value ?? "-",
							}))}
						/>
						<ExpandableSection headerText="Target input" defaultExpanded>
							<CodeBlock text={detail.Target?.Input ?? ""} />
						</ExpandableSection>
						<ExpandableSection headerText="JSON definition">
							<CodeBlock text={JSON.stringify(detail, null, 2)} />
						</ExpandableSection>
					</SpaceBetween>
				</Modal>
			)}
			{(dialog === "create" || dialog === "edit") && (
				<ScheduleDialog
					scope={scope}
					creating={dialog === "create"}
					initial={form}
					groups={groups}
					resources={resources}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						onChanged();
					}}
				/>
			)}
			{dialog === "delete" && target && (
				<ActionDialog
					title="Delete schedule"
					submit="Delete schedule"
					confirmation={target.Name}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						onChanged();
					}}
					plan={async () => [
						{
							label: "DeleteSchedule",
							run: () =>
								schedulerCall(scope, "DeleteSchedule", {
									Name: target.Name,
									GroupName: target.GroupName,
								}),
						},
					]}
				>
					<Alert type="warning">
						{target.Name} in group {target.GroupName} stops running and cannot
						be restored.
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}

function GroupFilter({
	value,
	groups,
	onChange,
}: {
	value: string;
	groups: ScheduleGroup[];
	onChange: (value: string) => void;
}) {
	return (
		<Select
			ariaLabel="Filter by group"
			placeholder="Choose a group"
			selectedOption={
				value ? { value, label: value } : { value: "", label: "All groups" }
			}
			options={[
				{ value: "", label: "All groups" },
				...groups.map((g) => ({ value: g.Name, label: g.Name })),
			]}
			onChange={({ detail }) => onChange(detail.selectedOption.value ?? "")}
		/>
	);
}

function ScheduleDialog({
	scope,
	creating,
	initial,
	groups,
	resources,
	onClose,
	onSaved,
}: {
	scope: SchedulerScope;
	creating: boolean;
	initial: ScheduleForm;
	groups: ScheduleGroup[];
	resources: ArnResource[];
	onClose: () => void;
	onSaved: () => void;
}) {
	const [f, setF] = useState(initial);
	const set = (patch: Partial<ScheduleForm>) => setF({ ...f, ...patch });
	const options = useMemo(
		() => targetOptions(resources, scope),
		[resources, scope],
	);
	const service = targetService(f.targetArn.trim());
	const hint = expressionHints[f.kind];
	const operation = creating ? "CreateSchedule" : "UpdateSchedule";
	return (
		<ActionDialog
			title={creating ? "Create schedule" : `Edit ${initial.name}`}
			submit={creating ? "Create schedule" : "Save schedule"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = scheduleInput(f);
				return [
					{
						label: operation,
						run: () => schedulerCall(scope, operation, input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				{!creating && (
					<Alert type="info">
						An update replaces the whole schedule, so every field below is sent
						again.
					</Alert>
				)}
				<Text
					label="Schedule name"
					value={f.name}
					disabled={!creating}
					onChange={(name) => set({ name })}
					constraint="1 to 64 letters, digits, hyphens, underscores and periods."
				/>
				<Pick
					label="Schedule group"
					value={f.group}
					disabled={!creating}
					placeholder="Choose a group"
					options={[
						{ value: defaultGroup },
						...groups
							.filter((g) => g.Name !== defaultGroup)
							.map((g) => ({ value: g.Name })),
					]}
					onChange={(group) => set({ group })}
				/>
				<Text
					label="Description"
					value={f.description}
					onChange={(description) => set({ description })}
				/>
				<FormField label="Schedule type" description={hint.description}>
					<RadioGroup
						ariaLabel="Schedule type"
						value={f.kind}
						items={(["rate", "cron", "at"] as ExpressionKind[]).map((k) => ({
							value: k,
							label: expressionHints[k].label,
						}))}
						onChange={({ detail }) =>
							set({ kind: detail.value as ExpressionKind })
						}
					/>
				</FormField>
				{f.kind === "rate" && (
					<SpaceBetween size="m" direction="horizontal">
						<Text
							label="Run every"
							value={f.rateValue}
							onChange={(rateValue) => set({ rateValue })}
							constraint="A whole number, such as 5."
						/>
						<Pick
							label="Unit"
							value={f.rateUnit}
							placeholder="Choose a unit"
							options={[
								{ value: "minute", label: "minutes" },
								{ value: "hour", label: "hours" },
								{ value: "day", label: "days" },
							]}
							onChange={(rateUnit) => set({ rateUnit: rateUnit as RateUnit })}
						/>
					</SpaceBetween>
				)}
				{f.kind === "cron" && (
					<Text
						label="Cron expression"
						value={f.cron}
						onChange={(cron) => set({ cron })}
						description={`Example: ${hint.placeholder} runs every day at 12:00.`}
						constraint="minutes hours day-of-month month day-of-week year"
					/>
				)}
				{f.kind === "at" && (
					<Text
						label="Run once at"
						value={f.at}
						onChange={(at) => set({ at })}
						description={`Example: ${hint.placeholder}`}
						constraint="Local time in the time zone below."
					/>
				)}
				<Text
					label="Time zone"
					value={f.timezone}
					onChange={(timezone) => set({ timezone })}
					description="An IANA name such as UTC, Europe/Berlin or America/New_York."
				/>
				<SpaceBetween size="m" direction="horizontal">
					<Text
						label="Start date (optional)"
						value={f.start}
						onChange={(start) => set({ start })}
						constraint="2030-01-31T09:30:00Z"
					/>
					<Text
						label="End date (optional)"
						value={f.end}
						onChange={(end) => set({ end })}
						constraint="2030-12-31T09:30:00Z"
					/>
				</SpaceBetween>
				<SpaceBetween size="m" direction="horizontal">
					<Pick
						label="Flexible time window"
						value={f.window}
						placeholder="Choose a mode"
						options={[
							{ value: "OFF", label: "Off" },
							{ value: "FLEXIBLE", label: "Flexible" },
						]}
						onChange={(window) =>
							set({ window: window as ScheduleForm["window"] })
						}
					/>
					{f.window === "FLEXIBLE" && (
						<Text
							label="Maximum window (minutes)"
							value={f.windowMinutes}
							onChange={(windowMinutes) => set({ windowMinutes })}
							constraint="1 to 1440"
						/>
					)}
					<Pick
						label="State"
						value={f.state}
						placeholder="Choose a state"
						options={[
							{ value: "ENABLED", label: "Enabled" },
							{ value: "DISABLED", label: "Disabled" },
						]}
						onChange={(state) => set({ state: state as ScheduleForm["state"] })}
					/>
					<Pick
						label="Action after completion"
						value={f.after}
						placeholder="Choose an action"
						description="Delete removes a schedule once it has finished its last run."
						options={[
							{ value: "NONE", label: "None" },
							{ value: "DELETE", label: "Delete the schedule" },
						]}
						onChange={(after) => set({ after: after as ScheduleForm["after"] })}
					/>
				</SpaceBetween>
				<Suggest
					label="Target ARN"
					value={f.targetArn}
					options={options.targets}
					placeholder="Choose a resource or enter an ARN"
					description="A Lambda function, SQS queue, SNS topic, EventBridge bus, Step Functions state machine or Kinesis stream, or a universal target."
					constraint="Universal targets look like arn:aws:scheduler:::aws-sdk:<service>:<apiAction>."
					onChange={(targetArn) => set({ targetArn })}
				/>
				<Suggest
					label="Role ARN"
					value={f.roleArn}
					options={options.roles}
					placeholder="Choose a role or enter an ARN"
					description="The role Scheduler assumes to invoke the target."
					onChange={(roleArn) => set({ roleArn })}
				/>
				<FormField
					label="Target input (JSON, optional)"
					description="Sent to the target as the payload."
				>
					<Textarea
						ariaLabel="Target input (JSON, optional)"
						rows={4}
						value={f.input}
						placeholder='{"key": "value"}'
						onChange={({ detail }) => set({ input: detail.value })}
					/>
				</FormField>
				{service === "events" && (
					<SpaceBetween size="m" direction="horizontal">
						<Text
							label="Event detail type"
							value={f.detailType}
							onChange={(detailType) => set({ detailType })}
						/>
						<Text
							label="Event source"
							value={f.source}
							onChange={(source) => set({ source })}
						/>
					</SpaceBetween>
				)}
				{service === "kinesis" && (
					<Text
						label="Partition key"
						value={f.partitionKey}
						onChange={(partitionKey) => set({ partitionKey })}
					/>
				)}
				{service === "sqs" && (
					<Text
						label="Message group ID (optional)"
						value={f.messageGroupId}
						onChange={(messageGroupId) => set({ messageGroupId })}
						description="Only for FIFO queues."
					/>
				)}
				<SpaceBetween size="m" direction="horizontal">
					<Text
						label="Maximum event age in seconds (optional)"
						value={f.retryAge}
						onChange={(retryAge) => set({ retryAge })}
						constraint="60 to 86400"
					/>
					<Text
						label="Maximum retry attempts (optional)"
						value={f.retryAttempts}
						onChange={(retryAttempts) => set({ retryAttempts })}
						constraint="0 to 185"
					/>
				</SpaceBetween>
				<Suggest
					label="Dead-letter queue ARN (optional)"
					value={f.dlqArn}
					options={options.queues}
					placeholder="Choose a queue or enter an ARN"
					description="Failed runs are sent to this SQS queue."
					onChange={(dlqArn) => set({ dlqArn })}
				/>
				<Text
					label="KMS key ARN (optional)"
					value={f.kmsKeyArn}
					onChange={(kmsKeyArn) => set({ kmsKeyArn })}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}
