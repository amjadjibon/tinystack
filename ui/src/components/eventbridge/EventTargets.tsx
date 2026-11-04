import {
	Alert,
	Box,
	Button,
	FormField,
	Header,
	Hotspot,
	Input,
	Select,
	SpaceBetween,
	Table,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import type { Resource } from "#/lib/api";
import { jsonObject } from "#/lib/consoleActions";
import {
	checkEntries,
	type EventScope,
	type EventTarget,
	eventCall,
} from "#/lib/eventbridge";
import { HOTSPOT } from "#/tutorials/hotspots";
export function EventTargets({
	scope,
	targets,
	resources,
	enabled,
	onChanged,
}: {
	scope: EventScope;
	targets: EventTarget[];
	resources: Resource[];
	enabled: boolean;
	onChanged: (message: string) => void;
}) {
	const available = resources.filter(
		(r) =>
			r.account === scope.account &&
			r.region === scope.region &&
			((r.service === "sqs" && r.type === "queue") ||
				(r.service === "sns" && r.type === "topic") ||
				(r.service === "lambda" && r.type === "function")),
	);
	const [dialog, setDialog] = useState<"add" | "edit" | "remove" | null>(null),
		[selected, setSelected] = useState<EventTarget>();
	const [id, setId] = useState(""),
		[arn, setArn] = useState(""),
		[options, setOptions] = useState("{}");
	const open = (target?: EventTarget) => {
		setSelected(target);
		setId(target?.Id ?? "");
		setArn(target?.Arn ?? available[0]?.arn ?? "");
		const { Id, Arn, ...rest } = target ?? { Id: "", Arn: "" };
		setOptions(JSON.stringify(rest, null, 2));
		setDialog(target ? "edit" : "add");
	};
	const changed = () => {
		setDialog(null);
		onChanged(dialog === "remove" ? "Target removed." : "Target saved.");
	};
	return (
		<SpaceBetween size="l">
			<Table
				items={targets}
				trackBy="Id"
				wrapLines
				header={
					<Header
						variant="h2"
						counter={`(${targets.length})`}
						description="Up to five Lambda, SQS or SNS targets in the rule's account and Region."
						actions={
							<Hotspot
								hotspotId={HOTSPOT.eventsAddTarget}
								side="left"
								direction="left"
							>
								<Button
									variant="primary"
									disabled={!enabled || targets.length >= 5}
									onClick={() => open()}
								>
									Add target
								</Button>
							</Hotspot>
						}
					>
						Targets
					</Header>
				}
				columnDefinitions={[
					{ id: "id", header: "Target ID", cell: (t) => t.Id },
					{ id: "arn", header: "Target ARN", cell: (t) => t.Arn },
					{
						id: "input",
						header: "Input",
						cell: (t) =>
							t.Input
								? "Constant JSON"
								: t.InputPath ||
									(t.InputTransformer ? "Input transformer" : "Matched event"),
					},
					{
						id: "dlq",
						header: "Dead-letter queue",
						cell: (t) => t.DeadLetterConfig?.Arn || "—",
					},
					{
						id: "actions",
						header: "Actions",
						cell: (t) => (
							<SpaceBetween direction="horizontal" size="xs">
								<Button disabled={!enabled} onClick={() => open(t)}>
									Edit
								</Button>
								<Button
									disabled={!enabled}
									onClick={() => {
										setSelected(t);
										setDialog("remove");
									}}
								>
									Remove
								</Button>
							</SpaceBetween>
						),
					},
				]}
				empty={
					<Box textAlign="center" padding="l">
						Add a target to deliver events matched by this rule.
					</Box>
				}
			/>
			{dialog && (
				<ActionDialog
					title={
						dialog === "remove"
							? "Remove target"
							: dialog === "add"
								? "Add target"
								: "Edit target"
					}
					submit={dialog === "remove" ? "Remove target" : "Save target"}
					confirmation={dialog === "remove" ? selected?.Id : undefined}
					onClose={() => setDialog(null)}
					onSaved={changed}
					plan={async () => {
						if (dialog === "remove")
							return [
								{
									label: "Remove target",
									run: async () =>
										checkEntries(
											await eventCall(scope, "RemoveTargets", {
												EventBusName: scope.bus,
												Rule: scope.rule,
												Ids: [selected?.Id],
											}),
										),
								},
							];
						if (!/^[A-Za-z0-9_.-]{1,64}$/.test(id))
							throw new Error(
								"Target IDs use 1–64 letters, numbers, underscores, hyphens or periods.",
							);
						if (dialog === "add" && targets.some((t) => t.Id === id))
							throw new Error(
								"This target ID already exists. Edit it instead.",
							);
						const extra = jsonObject(options, "Target options");
						if (Object.hasOwn(extra, "Id") || Object.hasOwn(extra, "Arn"))
							throw new Error("Set the target ID and ARN using their fields.");
						const input = { Id: id, Arn: arn, ...extra };
						return [
							{
								label: "Save target",
								run: async () =>
									checkEntries(
										await eventCall(scope, "PutTargets", {
											EventBusName: scope.bus,
											Rule: scope.rule,
											Targets: [input],
										}),
									),
							},
						];
					}}
				>
					{dialog === "remove" ? (
						<Alert type="warning">
							Remove {selected?.Id} from this rule. Its destination resource
							remains.
						</Alert>
					) : (
						<SpaceBetween size="l">
							<FormField label="Target ID">
								<Input
									ariaLabel="Target ID"
									disabled={dialog === "edit"}
									value={id}
									onChange={({ detail }) => setId(detail.value)}
								/>
							</FormField>
							<FormField label="Choose a destination — optional">
								<Select
									placeholder="Choose Lambda, SQS or SNS"
									selectedOption={
										available.some((r) => r.arn === arn)
											? {
													value: arn,
													label: available.find((r) => r.arn === arn)?.name,
												}
											: null
									}
									options={available.map((r) => ({
										value: r.arn,
										label: `${r.service.toUpperCase()} · ${r.name}`,
									}))}
									onChange={({ detail }) =>
										setArn(detail.selectedOption.value ?? "")
									}
								/>
							</FormField>
							<FormField label="Target ARN">
								<Input
									ariaLabel="Target ARN"
									value={arn}
									onChange={({ detail }) => setArn(detail.value)}
								/>
							</FormField>
							<FormField
								label="Target options (JSON)"
								description="Optional Input (a JSON string), InputPath, InputTransformer, RoleArn, RetryPolicy, DeadLetterConfig or SqsParameters. Use {} to send the matched event with default retries."
							>
								<Textarea
									ariaLabel="Target options"
									rows={10}
									value={options}
									onChange={({ detail }) => setOptions(detail.value)}
								/>
							</FormField>
							<Alert type="info">
								Choose one of Input, InputPath or InputTransformer. FIFO SQS
								targets need SqsParameters.MessageGroupId. Dead-letter
								destinations must be standard SQS queues in this account and
								Region. Existing options are replaced when saved.
							</Alert>
						</SpaceBetween>
					)}
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
