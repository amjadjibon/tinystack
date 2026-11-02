import {
	FormField,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Choice } from "#/components/containers/Controls";
import { Text } from "#/components/memorydb/Fields";
import { type PipesScope, pipesCall } from "#/lib/pipes";
import { type PipeForm, pipeInput } from "#/lib/pipesHelpers";

export function PipeDialog({
	scope,
	initial,
	creating,
	onClose,
	onSaved,
}: {
	scope: PipesScope;
	initial: PipeForm;
	creating: boolean;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [form, setForm] = useState(initial);
	const set = (key: keyof PipeForm, value: string) =>
		setForm((f) => ({ ...f, [key]: value }));
	const operation = creating ? "CreatePipe" : "UpdatePipe";
	const jsonFields: { key: keyof PipeForm; label: string; hint: string }[] = [
		{
			key: "sourceParameters",
			label: "Source parameters (JSON)",
			hint: `FilterCriteria: {"Filters":[{"Pattern":"{\\"body\\":{\\"type\\":[\\"order\\"]}}"}]}. Use FilterCriteria: {"Filters":[]} to clear filters when editing. SqsQueueParameters, KinesisStreamParameters or DynamoDBStreamParameters accept BatchSize and MaximumBatchingWindowInSeconds (only 0). ${creating ? 'Stream sources require StartingPosition: "LATEST" or "TRIM_HORIZON".' : "Stream starting position is preserved and cannot be edited."}`,
		},
		{
			key: "enrichmentParameters",
			label: "Enrichment parameters (JSON)",
			hint: "Optional InputTemplate string. Use {} when no enrichment is configured.",
		},
		{
			key: "targetParameters",
			label: "Target parameters (JSON)",
			hint: "Optional InputTemplate. LambdaFunctionParameters: InvocationType REQUEST_RESPONSE or FIRE_AND_FORGET. StepFunctionStateMachineParameters: InvocationType FIRE_AND_FORGET. KinesisStreamParameters: required PartitionKey. SqsQueueParameters: MessageGroupId and MessageDeduplicationId. EventBridgeEventBusParameters: literal DetailType and Source.",
		},
		...(creating
			? [
					{
						key: "tags" as const,
						label: "Tags (JSON)",
						hint: 'Object of string values, such as {"environment":"dev"}.',
					},
				]
			: []),
	];
	return (
		<ActionDialog
			title={creating ? "Create pipe" : `Edit ${initial.name}`}
			submit={creating ? "Create pipe" : "Save pipe"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = pipeInput(form, creating);
				return [
					{ label: operation, run: () => pipesCall(scope, operation, input) },
				];
			}}
		>
			<SpaceBetween size="m">
				<Text
					label="Name"
					value={form.name}
					disabled={!creating}
					onChange={(v) => set("name", v)}
					constraint="1–64 letters, digits, periods, hyphens and underscores."
				/>
				<Text
					label="Description"
					value={form.description}
					onChange={(v) => set("description", v)}
				/>
				<Text
					label="Source ARN"
					value={form.source}
					disabled={!creating}
					onChange={(v) => set("source", v)}
					description="An existing SQS queue, Kinesis stream or DynamoDB stream in this account and Region."
				/>
				<Text
					label="Enrichment ARN (optional)"
					value={form.enrichment}
					onChange={(v) => set("enrichment", v)}
					description="A Lambda function in this account and Region."
				/>
				<Text
					label="Target ARN"
					value={form.target}
					onChange={(v) => set("target", v)}
					description="Lambda, SQS, SNS, EventBridge event bus, Step Functions state machine or Kinesis stream in this account and Region."
				/>
				<Text
					label="Role ARN"
					value={form.role}
					onChange={(v) => set("role", v)}
				/>
				<Choice
					label="Desired state"
					value={form.desiredState}
					options={["STOPPED", "RUNNING"]}
					onChange={(v) => set("desiredState", v)}
				/>
				{jsonFields.map(({ key, label, hint }) => (
					<FormField key={key} label={label} description={hint}>
						<Textarea
							ariaLabel={label}
							rows={6}
							value={form[key]}
							onChange={({ detail }) => set(key, detail.value)}
						/>
					</FormField>
				))}
			</SpaceBetween>
		</ActionDialog>
	);
}
