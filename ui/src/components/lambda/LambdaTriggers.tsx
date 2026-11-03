import {
	Alert,
	Autosuggest,
	Box,
	Button,
	Checkbox,
	FormField,
	Header,
	Input,
	Link,
	RadioGroup,
	SpaceBetween,
	Table,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { SNSSubscriptionForm } from "#/components/sns/SNSSubscriptionForm";
import type { Resource } from "#/lib/api";
import { integer, jsonObject } from "#/lib/consoleActions";
import {
	functionArn,
	type LambdaMapping,
	type LambdaScope,
	lambdaCall,
} from "#/lib/lambda";
import {
	type SNSSubscription,
	snsCall,
	snsHref,
	topicSubscriptions,
} from "#/lib/sns";
import { useFollow } from "#/lib/useFollow";

type Trigger = {
	id: string;
	kind: "mapping" | "sns";
	source: string;
	state: string;
	mapping?: LambdaMapping;
	subscription?: SNSSubscription;
};
export function LambdaTriggers({
	scope,
	resources,
	enabled,
	snsEnabled,
	loading,
	onRefresh,
	onChanged,
}: {
	scope: LambdaScope;
	resources: Resource[];
	enabled: boolean;
	snsEnabled: boolean;
	loading: boolean;
	onRefresh: () => void;
	onChanged: (message: string) => void;
}) {
	const follow = useFollow();
	const arn = functionArn(scope);
	const scoped = resources.filter(
		(r) => r.account === scope.account && r.region === scope.region,
	);
	const triggers: Trigger[] = [
		...scoped
			.filter(
				(r) => r.service === "lambda" && r.type === "event-source-mapping",
			)
			.map((r) => r.details as LambdaMapping)
			.filter(
				(m) => m.FunctionArn === arn || m.FunctionArn.startsWith(`${arn}:`),
			)
			.map((m) => ({
				id: m.UUID,
				kind: "mapping" as const,
				source: m.EventSourceArn,
				state: m.State,
				mapping: m,
			})),
		...resources
			.filter((r) => r.service === "sns" && r.type === "topic")
			.flatMap(topicSubscriptions)
			.filter(
				(s) =>
					s.Protocol === "lambda" &&
					(s.Endpoint === arn || s.Endpoint.startsWith(`${arn}:`)),
			)
			.map((s) => ({
				id: s.SubscriptionArn,
				kind: "sns" as const,
				source: s.TopicArn,
				state: s.PendingConfirmation ? "Pending" : "Active",
				subscription: s,
			})),
	];
	const [dialog, setDialog] = useState<"add" | "edit" | "delete" | null>(null);
	const [selected, setSelected] = useState<Trigger>();
	const [kind, setKind] = useState("sqs");
	const [source, setSource] = useState("");
	const [batch, setBatch] = useState("10");
	const [window, setWindow] = useState("0");
	const [active, setActive] = useState(true);
	const [partial, setPartial] = useState(false);
	const [filters, setFilters] = useState("[]");
	const open = (mode: "add" | "edit" | "delete", trigger?: Trigger) => {
		setSelected(trigger);
		setDialog(mode);
		setSource(trigger?.source ?? "");
		setKind("sqs");
		setBatch(String(trigger?.mapping?.BatchSize ?? 10));
		setWindow(String(trigger?.mapping?.MaximumBatchingWindowInSeconds ?? 0));
		setActive(trigger?.mapping ? trigger.mapping.State === "Enabled" : true);
		setPartial(
			trigger?.mapping?.FunctionResponseTypes?.includes(
				"ReportBatchItemFailures",
			) ?? false,
		);
		setFilters(
			JSON.stringify(
				trigger?.mapping?.FilterCriteria?.Filters?.map((f) =>
					JSON.parse(f.Pattern),
				) ?? [],
				null,
				2,
			),
		);
	};
	const saved = () => {
		setDialog(null);
		onChanged(
			dialog === "delete"
				? "Trigger deleted."
				: dialog === "edit"
					? "Trigger updated."
					: "Trigger added.",
		);
	};
	const options = scoped
		.filter((r) =>
			kind === "sns"
				? r.service === "sns" && r.type === "topic" && !r.name.endsWith(".fifo")
				: r.service === "sqs" && r.type === "queue",
		)
		.map((r) => ({ value: r.arn, label: r.name, description: r.arn }));
	return (
		<SpaceBetween size="l">
			<Table
				items={triggers}
				trackBy="id"
				loading={loading}
				loadingText="Loading triggers"
				wrapLines
				header={
					<Header
						variant="h2"
						counter={`(${triggers.length})`}
						description="Queue and stream event source mappings and SNS subscriptions connected to this function."
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									iconName="refresh"
									ariaLabel="Refresh triggers"
									onClick={onRefresh}
								/>
								<Button
									variant="primary"
									disabled={!enabled}
									onClick={() => open("add")}
								>
									Add trigger
								</Button>
							</SpaceBetween>
						}
					>
						Triggers
					</Header>
				}
				columnDefinitions={[
					{
						id: "source",
						header: "Source",
						cell: (t) =>
							t.kind === "sns" && t.subscription ? (
								<Link
									href={snsHref(
										{
											account: t.source.split(":")[4],
											region: t.source.split(":")[3],
											topic: t.source.split(":")[5],
										},
										t.id,
									)}
									onFollow={follow}
								>
									{t.source}
								</Link>
							) : (
								t.source
							),
					},
					{
						id: "type",
						header: "Type",
						cell: (t) =>
							t.kind === "sns"
								? "SNS subscription"
								: t.source.includes(":sqs:")
									? "SQS"
									: "Stream mapping",
					},
					{ id: "state", header: "State", cell: (t) => t.state },
					{
						id: "settings",
						header: "Processing",
						cell: (t) =>
							t.mapping
								? `${t.mapping.BatchSize} records · ${t.mapping.LastProcessingResult ?? "No records processed"}`
								: "Asynchronous invocation",
					},
					{
						id: "actions",
						header: "Actions",
						cell: (t) => (
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									disabled={t.kind === "sns" ? !snsEnabled : !enabled}
									onClick={() => open("edit", t)}
								>
									Edit
								</Button>
								<Button
									disabled={t.kind === "sns" ? !snsEnabled : !enabled}
									onClick={() => open("delete", t)}
								>
									Delete
								</Button>
							</SpaceBetween>
						),
					},
				]}
				empty={
					<Box padding="l" textAlign="center">
						No triggers. Add an SQS queue or a standard SNS topic to invoke this
						function.
					</Box>
				}
			/>
			{dialog === "edit" && selected?.subscription ? (
				<SNSSubscriptionForm
					scope={{
						account: selected.source.split(":")[4],
						region: selected.source.split(":")[3],
						topic: selected.source.split(":")[5],
					}}
					resources={resources}
					fifo={false}
					subscription={selected.subscription}
					onClose={() => setDialog(null)}
					onSaved={saved}
				/>
			) : (
				dialog && (
					<ActionDialog
						title={
							dialog === "add"
								? "Add trigger"
								: dialog === "edit"
									? "Edit trigger"
									: "Delete trigger"
						}
						submit={
							dialog === "add"
								? "Add trigger"
								: dialog === "delete"
									? "Delete trigger"
									: "Save changes"
						}
						confirmation={dialog === "delete" ? "delete" : undefined}
						onClose={() => setDialog(null)}
						onSaved={saved}
						plan={async () => {
							if (dialog === "delete" && selected)
								return [
									{
										label: "Delete trigger",
										run: () =>
											selected.kind === "sns"
												? snsCall(
														{
															account: selected.source.split(":")[4],
															region: selected.source.split(":")[3],
														},
														"Unsubscribe",
														{ SubscriptionArn: selected.id },
													)
												: lambdaCall(scope, "DeleteEventSourceMapping", {
														UUID: selected.id,
													}),
									},
								];
							if (dialog === "add" && kind === "sns") {
								if (!snsEnabled)
									throw new Error("SNS console actions are unavailable.");
								if (!options.some((o) => o.value === source))
									throw new Error(
										"Choose a standard SNS topic in this account and Region.",
									);
								return [
									{
										label: "Subscribe function",
										run: () =>
											snsCall(scope, "Subscribe", {
												TopicArn: source,
												Protocol: "lambda",
												Endpoint: arn,
												ReturnSubscriptionArn: true,
											}),
									},
								];
							}
							if (dialog === "add" && !options.some((o) => o.value === source))
								throw new Error(
									"Choose an SQS queue in this account and Region.",
								);
							const patterns: unknown = JSON.parse(filters);
							if (!Array.isArray(patterns) || patterns.length > 5)
								throw new Error(
									"Event filters must be a JSON array of up to five objects.",
								);
							const FilterCriteria = {
								Filters: patterns.map((p) => ({
									Pattern: JSON.stringify(
										jsonObject(JSON.stringify(p), "Event filter"),
									),
								})),
							};
							const input = {
								Enabled: active,
								BatchSize: integer(
									batch,
									"Batch size",
									1,
									source.endsWith(".fifo")
										? 10
										: source.includes(":sqs:")
											? 10000
											: 10000,
								),
								MaximumBatchingWindowInSeconds: integer(
									window,
									"Batching window",
									0,
									300,
								),
								FunctionResponseTypes: partial
									? ["ReportBatchItemFailures"]
									: [],
								FilterCriteria,
							};
							return [
								{
									label: "Save event source mapping",
									run: () =>
										lambdaCall(
											scope,
											dialog === "edit"
												? "UpdateEventSourceMapping"
												: "CreateEventSourceMapping",
											dialog === "edit"
												? { UUID: selected?.id, ...input }
												: {
														FunctionName: scope.functionName,
														EventSourceArn: source,
														...input,
													},
										),
								},
							];
						}}
					>
						{dialog === "delete" ? (
							<Alert type="warning">
								Remove this connection to {source}. The source resource and any
								messages already processed remain in place.
							</Alert>
						) : (
							<SpaceBetween size="l">
								{dialog === "add" && (
									<FormField label="Trigger type">
										<RadioGroup
											value={kind}
											onChange={({ detail }) => {
												setKind(detail.value);
												setSource("");
											}}
											items={[
												{ value: "sqs", label: "SQS" },
												{ value: "sns", label: "SNS", disabled: !snsEnabled },
											]}
										/>
									</FormField>
								)}
								<FormField
									label={kind === "sns" ? "SNS topic" : "Event source ARN"}
								>
									<Autosuggest
										ariaLabel="Trigger source ARN"
										value={source}
										disabled={dialog === "edit"}
										options={options}
										onChange={({ detail }) => setSource(detail.value)}
									/>
								</FormField>
								{kind === "sns" ? (
									<Alert type="info">
										Messages published to this topic invoke the function
										asynchronously. Configure subscription filters after adding
										the trigger.
									</Alert>
								) : (
									<SpaceBetween size="l">
										<Alert type="info">
											An enabled mapping polls its source and invokes your
											function. SQS messages are deleted after successful
											processing; filtered messages are also removed.
										</Alert>
										<Checkbox
											checked={active}
											onChange={({ detail }) => setActive(detail.checked)}
										>
											Enable trigger
										</Checkbox>
										<FormField
											label="Batch size"
											constraintText={
												source && !source.includes(":sqs:")
													? "1–10,000 records for stream mappings."
													: "1–10,000 for standard SQS; up to 10 for FIFO. Batches over 10 require a batching window of at least 1 second."
											}
										>
											<Input
												ariaLabel="Trigger batch size"
												type="number"
												value={batch}
												onChange={({ detail }) => setBatch(detail.value)}
											/>
										</FormField>
										<FormField
											label="Batching window (seconds)"
											constraintText="0–300 seconds"
										>
											<Input
												ariaLabel="Trigger batching window"
												type="number"
												value={window}
												onChange={({ detail }) => setWindow(detail.value)}
											/>
										</FormField>
										<Checkbox
											checked={partial}
											onChange={({ detail }) => setPartial(detail.checked)}
										>
											Report individual batch item failures
										</Checkbox>
										<FormField
											label="Event filters (JSON)"
											description='An array of up to five filter objects, for example [{"body":{"kind":["created"]}}]. Use [] to remove filters.'
										>
											<Textarea
												ariaLabel="Trigger event filters"
												value={filters}
												rows={6}
												onChange={({ detail }) => setFilters(detail.value)}
											/>
										</FormField>
									</SpaceBetween>
								)}
							</SpaceBetween>
						)}
					</ActionDialog>
				)
			)}
		</SpaceBetween>
	);
}
