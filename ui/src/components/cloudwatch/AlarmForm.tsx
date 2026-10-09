import {
	Alert,
	Button,
	Checkbox,
	ColumnLayout,
	FormField,
	Input,
	Select,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import {
	type Alarm,
	alarmInput,
	alarmTypes,
	comparisons,
	dimensions,
	finite,
	type Metric,
	periodValue,
	statistics,
	tagsInput,
	type WatchScope,
	watchCall,
} from "#/lib/cloudwatch";
import { integer, stringMap } from "#/lib/consoleActions";
import { useScope } from "#/lib/scope";

export function AlarmForm({
	scope,
	initial,
	metric,
	onClose,
	onSaved,
}: {
	scope: WatchScope;
	initial?: Alarm;
	metric?: Metric;
	onClose: () => void;
	onSaved: (name: string) => void;
}) {
	const { inventory } = useScope();
	const [type, setType] = useState(initial?.Type ?? "MetricAlarm"),
		[name, setName] = useState(initial?.AlarmName ?? ""),
		[description, setDescription] = useState(initial?.AlarmDescription ?? "");
	const [namespace, setNamespace] = useState(
			initial?.Namespace ?? metric?.Namespace ?? "",
		),
		[metricName, setMetricName] = useState(
			initial?.MetricName ?? metric?.MetricName ?? "",
		);
	const [dims, setDims] = useState(
		JSON.stringify(
			Object.fromEntries(
				(initial?.Dimensions ?? metric?.Dimensions ?? []).map((d) => [
					d.Name,
					d.Value,
				]),
			),
			null,
			2,
		),
	);
	const [stat, setStat] = useState(
			initial?.ExtendedStatistic ?? initial?.Statistic ?? "Average",
		),
		[period, setPeriod] = useState(String(initial?.Period ?? 60));
	const [evaluation, setEvaluation] = useState(
			String(initial?.EvaluationPeriods ?? 1),
		),
		[points, setPoints] = useState(
			String(initial?.DatapointsToAlarm ?? initial?.EvaluationPeriods ?? 1),
		);
	const [threshold, setThreshold] = useState(String(initial?.Threshold ?? 1)),
		[comparison, setComparison] = useState(
			initial?.ComparisonOperator ?? comparisons[0],
		),
		[missing, setMissing] = useState(initial?.TreatMissingData ?? "missing");
	const [rule, setRule] = useState(initial?.AlarmRule ?? ""),
		[actionsEnabled, setActionsEnabled] = useState(
			initial?.ActionsEnabled ?? true,
		),
		[tags, setTags] = useState("{}");
	const [actions, setActions] = useState({
		AlarmActions: (initial?.AlarmActions ?? []).join("\n"),
		OKActions: (initial?.OKActions ?? []).join("\n"),
		InsufficientDataActions: (initial?.InsufficientDataActions ?? []).join(
			"\n",
		),
	});
	const [queries, setQueries] = useState(
			JSON.stringify(initial?.Metrics ?? [], null, 2),
		),
		[target, setTarget] = useState("");
	const targets = (inventory.data ?? [])
		.filter(
			(r) =>
				r.account === scope.account &&
				r.region === scope.region &&
				((r.service === "sns" && r.type === "topic") ||
					(r.service === "lambda" && r.type === "function")),
		)
		.map((r) => ({
			value: r.arn,
			label: `${r.service === "sns" ? "SNS" : "Lambda"} · ${r.name}`,
		}));
	const field = (
		label: string,
		value: string,
		set: (v: string) => void,
		description?: string,
	) => (
		<FormField label={label} description={description}>
			<Input
				ariaLabel={label}
				value={value}
				onChange={({ detail }) => set(detail.value)}
			/>
		</FormField>
	);
	const optionField = (
		label: string,
		value: string,
		set: (v: string) => void,
		options: string[],
	) => (
		<FormField label={label}>
			<Select
				selectedOption={{ value, label: value }}
				options={options.map((value) => ({ value, label: value }))}
				onChange={({ detail }) => set(detail.selectedOption.value ?? value)}
			/>
		</FormField>
	);
	return (
		<ActionDialog
			title={initial ? "Edit alarm" : "Create alarm"}
			submit={initial ? "Save changes" : "Create alarm"}
			onClose={onClose}
			onSaved={() => onSaved(name)}
			plan={async () => {
				if (!name.trim() || name.length > 255)
					throw new Error("Alarm name must use 1–255 characters.");
				if (description.length > 1024)
					throw new Error("Description must use at most 1,024 characters.");
				const actionLists = Object.fromEntries(
					Object.entries(actions).map(([k, v]) => {
						const arns = v
							.split("\n")
							.map((s) => s.trim())
							.filter(Boolean);
						if (
							arns.length > 5 ||
							arns.some((a) => !/^arn:aws:(sns|lambda|autoscaling):/.test(a))
						)
							throw new Error(
								"Each state accepts up to five SNS, Lambda or Auto Scaling action ARNs.",
							);
						return [k, arns];
					}),
				);
				const input: Record<string, unknown> = {
					...(initial ? alarmInput(initial) : {}),
					AlarmName: name,
					AlarmDescription: description,
					ActionsEnabled: actionsEnabled,
					...actionLists,
				};
				if (type === "CompositeAlarm") {
					if (!rule.trim()) throw new Error("Enter a composite alarm rule.");
					input.AlarmRule = rule;
				} else {
					input.EvaluationPeriods = integer(
						evaluation,
						"Evaluation periods",
						1,
						10000,
					);
					input.DatapointsToAlarm = integer(
						points,
						"Datapoints to alarm",
						1,
						Number(input.EvaluationPeriods),
					);
					input.Threshold = finite(threshold, "Threshold");
					input.ComparisonOperator = comparison;
					input.TreatMissingData = missing;
					if (initial?.Metrics) {
						const q: unknown = JSON.parse(queries);
						if (!Array.isArray(q) || !q.length)
							throw new Error("Metric queries must be a nonempty JSON array.");
						input.Metrics = q;
					} else {
						if (!namespace.trim() || !metricName.trim())
							throw new Error("Namespace and metric name are required.");
						input.Namespace = namespace;
						input.MetricName = metricName;
						input.Dimensions = dimensions(dims);
						input.Period = periodValue(period, true);
						delete input.Statistic;
						delete input.ExtendedStatistic;
						input[stat.startsWith("p") ? "ExtendedStatistic" : "Statistic"] =
							stat;
					}
				}
				if (!initial) {
					input.Tags = tagsInput(stringMap(tags, "Tags"));
					const exists = await watchCall<{
						MetricAlarms?: unknown[];
						CompositeAlarms?: unknown[];
					}>(scope, "DescribeAlarms", {
						AlarmNames: [name],
						AlarmTypes: alarmTypes,
					});
					if (exists.MetricAlarms?.length || exists.CompositeAlarms?.length)
						throw new Error(
							"An alarm with this name already exists. Open it to edit.",
						);
				}
				return [
					{
						label: initial ? "Update alarm" : "Create alarm",
						run: () =>
							watchCall(
								scope,
								type === "CompositeAlarm"
									? "PutCompositeAlarm"
									: "PutMetricAlarm",
								input,
							),
					},
				];
			}}
		>
			<SpaceBetween size="l">
				{!initial &&
					optionField(
						"Alarm type",
						type,
						(v) => setType(v as Alarm["Type"]),
						alarmTypes,
					)}
				<FormField label="Alarm name">
					<Input
						ariaLabel="Alarm name"
						value={name}
						disabled={!!initial}
						onChange={({ detail }) => setName(detail.value)}
					/>
				</FormField>
				{field("Description", description, setDescription)}
				{type === "CompositeAlarm" ? (
					<FormField
						label="Alarm rule"
						description={
							'Combine existing alarms: ALARM("errors") AND NOT OK("latency"). Supports AND, OR, NOT, parentheses, TRUE and FALSE.'
						}
					>
						<Textarea
							ariaLabel="Composite alarm rule"
							value={rule}
							rows={5}
							onChange={({ detail }) => setRule(detail.value)}
						/>
					</FormField>
				) : (
					<SpaceBetween size="m">
						{initial?.Metrics ? (
							<FormField
								label="Metric queries (JSON)"
								description="Keep exactly one returned series. Editing preserves the alarm's metric math configuration."
							>
								<Textarea
									ariaLabel="Alarm metric queries"
									value={queries}
									rows={10}
									onChange={({ detail }) => setQueries(detail.value)}
								/>
							</FormField>
						) : (
							<>
								<ColumnLayout columns={2}>
									{field("Namespace", namespace, setNamespace)}
									{field("Metric name", metricName, setMetricName)}
								</ColumnLayout>
								<FormField
									label="Dimensions (JSON)"
									description='Exact metric dimensions, for example {"FunctionName":"handler"}. Use {} for none.'
								>
									<Textarea
										ariaLabel="Alarm dimensions"
										value={dims}
										onChange={({ detail }) => setDims(detail.value)}
									/>
								</FormField>
								<ColumnLayout columns={2}>
									{optionField("Statistic", stat, setStat, [
										...new Set([...statistics, stat]),
									])}
									{field(
										"Period (seconds)",
										period,
										setPeriod,
										"10, 20, 30, or a multiple of 60",
									)}
								</ColumnLayout>
							</>
						)}
						<ColumnLayout columns={2}>
							{optionField(
								"Comparison",
								comparison,
								setComparison,
								comparisons,
							)}
							{field("Threshold", threshold, setThreshold)}
						</ColumnLayout>
						<ColumnLayout columns={2}>
							{field("Evaluation periods", evaluation, setEvaluation)}
							{field("Datapoints to alarm", points, setPoints)}
						</ColumnLayout>
						{optionField("Missing data", missing, setMissing, [
							"missing",
							"ignore",
							"breaching",
							"notBreaching",
						])}
					</SpaceBetween>
				)}
				<FormField label="Actions">
					<Checkbox
						checked={actionsEnabled}
						onChange={({ detail }) => setActionsEnabled(detail.checked)}
					>
						Enable alarm actions
					</Checkbox>
				</FormField>
				<Alert type="info">
					State changes can invoke the configured actions. Disabling actions
					keeps alarm evaluation running.
				</Alert>
				<FormField label="Add an existing SNS topic or Lambda function to ALARM actions">
					<SpaceBetween direction="horizontal" size="xs">
						<Select
							selectedOption={targets.find((t) => t.value === target) ?? null}
							options={targets}
							placeholder="Choose a target"
							onChange={({ detail }) =>
								setTarget(detail.selectedOption.value ?? "")
							}
						/>
						<Button
							disabled={!target}
							onClick={() =>
								setActions((a) => ({
									...a,
									AlarmActions: [a.AlarmActions, target]
										.filter(Boolean)
										.join("\n"),
								}))
							}
						>
							Add action
						</Button>
					</SpaceBetween>
				</FormField>
				{(
					["AlarmActions", "OKActions", "InsufficientDataActions"] as const
				).map((k) => (
					<FormField
						key={k}
						label={
							k === "AlarmActions"
								? "ALARM actions"
								: k === "OKActions"
									? "OK actions"
									: "INSUFFICIENT_DATA actions"
						}
						description="One ARN per line, up to five. SNS, Lambda, or Auto Scaling."
					>
						<Textarea
							ariaLabel={k}
							value={actions[k]}
							onChange={({ detail }) =>
								setActions((a) => ({ ...a, [k]: detail.value }))
							}
						/>
					</FormField>
				))}
				{!initial && (
					<FormField label="Tags (JSON) — optional">
						<Textarea
							ariaLabel="Alarm tags"
							value={tags}
							onChange={({ detail }) => setTags(detail.value)}
						/>
					</FormField>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}
