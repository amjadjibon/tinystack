import {
	Alert,
	Box,
	Button,
	Checkbox,
	Container,
	ContentLayout,
	Form,
	FormField,
	Header,
	Input,
	RadioGroup,
	Select,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import type { Resource } from "#/lib/api";
import {
	errorText,
	integer,
	type QueueAttributes,
	queueCall,
	queueNumbers,
	redriveTarget,
	type SQSScope,
	sqsCall,
	stringMap,
} from "#/lib/sqs";

export function SQSQueueForm({
	scope,
	queues,
	attributes,
	onCancel,
	onSaved,
}: {
	scope: SQSScope;
	queues: Resource[];
	attributes?: QueueAttributes;
	onCancel: () => void;
	onSaved: (name: string) => void;
}) {
	const editing = !!attributes;
	const [name, setName] = useState(scope.queue ?? "");
	const [fifo, setFifo] = useState(attributes?.FifoQueue === "true");
	const [dedup, setDedup] = useState(
		attributes?.ContentBasedDeduplication === "true",
	);
	const [highThroughput, setHighThroughput] = useState(
		attributes?.FifoThroughputLimit === "perMessageGroupId",
	);
	const [numbers, setNumbers] = useState<Record<string, string>>(() =>
		Object.fromEntries(
			queueNumbers.map((f) => [f.key, attributes?.[f.key] ?? f.value]),
		),
	);
	const [encryption, setEncryption] = useState(
		attributes?.KmsMasterKeyId
			? "kms"
			: attributes?.SqsManagedSseEnabled === "false"
				? "none"
				: "sqs",
	);
	const [kms, setKms] = useState(attributes?.KmsMasterKeyId ?? "alias/aws/sqs");
	const [reuse, setReuse] = useState(
		attributes?.KmsDataKeyReusePeriodSeconds ?? "300",
	);
	const [dlq, setDlq] = useState(redriveTarget(attributes));
	const [count, setCount] = useState(() => {
		try {
			return String(
				JSON.parse(attributes?.RedrivePolicy ?? "{}").maxReceiveCount ?? 5,
			);
		} catch {
			return "5";
		}
	});
	const [tags, setTags] = useState("{}");
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const targets = queues
		.filter(
			(q) =>
				q.name !== scope.queue && (q.details?.FifoQueue === "true") === fifo,
		)
		.map((q) => ({ label: q.name, value: q.arn }));
	const save = async () => {
		if (busy) return;
		setBusy(true);
		setError("");
		try {
			if (
				!/^[A-Za-z0-9_-]{1,80}$/.test(name) &&
				!/^[A-Za-z0-9_-]{1,75}\.fifo$/.test(name)
			)
				throw new Error(
					"Use 1–80 letters, numbers, hyphens or underscores. FIFO names must end with .fifo.",
				);
			if (fifo !== name.endsWith(".fifo"))
				throw new Error(
					"FIFO queue names must end with .fifo. Standard queues cannot use this suffix.",
				);
			if (!editing && queues.some((q) => q.name === name))
				throw new Error(
					"This queue already exists in the selected account and Region.",
				);
			const next: QueueAttributes = Object.fromEntries(
				queueNumbers.map((f) => [
					f.key,
					String(integer(numbers[f.key], f.label, f.min, f.max)),
				]),
			);
			if (!editing) next.FifoQueue = String(fifo);
			if (fifo) {
				next.ContentBasedDeduplication = String(dedup);
				next.DeduplicationScope = highThroughput ? "messageGroup" : "queue";
				next.FifoThroughputLimit = highThroughput
					? "perMessageGroupId"
					: "perQueue";
			}
			next.SqsManagedSseEnabled = String(encryption === "sqs");
			next.KmsMasterKeyId = encryption === "kms" ? kms.trim() : "";
			if (encryption === "kms") {
				if (!kms.trim()) throw new Error("Enter a KMS key ID, ARN or alias.");
				next.KmsDataKeyReusePeriodSeconds = String(
					integer(reuse, "Data key reuse period", 60, 86400),
				);
			}
			next.RedrivePolicy = dlq
				? JSON.stringify({
						deadLetterTargetArn: dlq,
						maxReceiveCount: integer(count, "Maximum receives", 1, 1000),
					})
				: "";
			if (editing)
				await queueCall(scope, "SetQueueAttributes", { Attributes: next });
			else
				await sqsCall(scope, "CreateQueue", {
					QueueName: name,
					Attributes: next,
					Tags: stringMap(tags, "Tags"),
				});
			onSaved(name);
		} catch (err) {
			setError(errorText(err));
		} finally {
			setBusy(false);
		}
	};
	return (
		<ContentLayout
			header={
				<Header variant="h1">
					{editing ? `Edit ${name}` : "Create queue"}
				</Header>
			}
		>
			<Form
				actions={
					<SpaceBetween direction="horizontal" size="xs">
						<Button disabled={busy} onClick={onCancel}>
							Cancel
						</Button>
						<Button variant="primary" loading={busy} onClick={save}>
							{editing ? "Save changes" : "Create queue"}
						</Button>
					</SpaceBetween>
				}
				errorText={error}
			>
				<fieldset disabled={busy} style={{ border: 0, margin: 0, padding: 0 }}>
					<SpaceBetween size="l">
						<Container header={<Header variant="h2">Details</Header>}>
							<SpaceBetween size="l">
								<FormField
									label="Type"
									description="The queue type cannot be changed after creation."
								>
									<RadioGroup
										value={fifo ? "fifo" : "standard"}
										onChange={({ detail }) => {
											setFifo(detail.value === "fifo");
											setDlq("");
										}}
										items={[
											{
												value: "standard",
												label: "Standard",
												description:
													"At-least-once delivery and best-effort ordering.",
												disabled: editing,
											},
											{
												value: "fifo",
												label: "FIFO",
												description:
													"First-in-first-out delivery with message groups and deduplication.",
												disabled: editing,
											},
										]}
									/>
								</FormField>
								<FormField
									label="Name"
									constraintText={
										fifo
											? "Include the .fifo suffix. Maximum 80 characters."
											: "Maximum 80 characters. Letters, numbers, hyphens and underscores."
									}
								>
									<Input
										ariaLabel="Queue name"
										value={name}
										disabled={editing}
										onChange={({ detail }) => setName(detail.value)}
									/>
								</FormField>
								<Box>
									AWS account: {scope.account} · AWS Region: {scope.region}
								</Box>
							</SpaceBetween>
						</Container>
						<Container header={<Header variant="h2">Configuration</Header>}>
							<SpaceBetween size="l">
								{queueNumbers.map((f) => (
									<FormField
										key={f.key}
										label={f.label}
										constraintText={`${f.min}–${f.max} ${f.unit}`}
									>
										<Input
											ariaLabel={f.label}
											value={numbers[f.key]}
											type="number"
											onChange={({ detail }) =>
												setNumbers({ ...numbers, [f.key]: detail.value })
											}
										/>
									</FormField>
								))}
								{fifo && (
									<>
										<Checkbox
											checked={dedup}
											onChange={({ detail }) => setDedup(detail.checked)}
										>
											Content-based deduplication
										</Checkbox>
										<Checkbox
											checked={highThroughput}
											onChange={({ detail }) =>
												setHighThroughput(detail.checked)
											}
										>
											High throughput FIFO (deduplicate per message group)
										</Checkbox>
									</>
								)}
							</SpaceBetween>
						</Container>
						<Container header={<Header variant="h2">Encryption</Header>}>
							<SpaceBetween size="l">
								<RadioGroup
									value={encryption}
									onChange={({ detail }) => setEncryption(detail.value)}
									items={[
										{ value: "sqs", label: "Amazon SQS key (SSE-SQS)" },
										{ value: "kms", label: "AWS KMS key (SSE-KMS)" },
										{ value: "none", label: "Disabled" },
									]}
								/>
								{encryption === "kms" && (
									<>
										<FormField label="KMS key">
											<Input
												ariaLabel="KMS key"
												value={kms}
												onChange={({ detail }) => setKms(detail.value)}
											/>
										</FormField>
										<FormField
											label="Data key reuse period"
											constraintText="60–86400 seconds"
										>
											<Input
												ariaLabel="Data key reuse period"
												type="number"
												value={reuse}
												onChange={({ detail }) => setReuse(detail.value)}
											/>
										</FormField>
									</>
								)}
								<Box color="text-body-secondary">
									TinyStack stores messages as sent. SSE settings are retained;
									customer KMS keys are checked when messages are sent or
									received.
								</Box>
							</SpaceBetween>
						</Container>
						<Container header={<Header variant="h2">Dead-letter queue</Header>}>
							<SpaceBetween size="l">
								<FormField
									label="Dead-letter queue"
									description="Choose another queue of the same type in this account and Region."
								>
									<Select
										ariaLabel="Dead-letter queue"
										selectedOption={
											targets.find((t) => t.value === dlq) ?? {
												label: dlq || "Disabled",
												value: dlq,
											}
										}
										options={[{ label: "Disabled", value: "" }, ...targets]}
										onChange={({ detail }) =>
											setDlq(detail.selectedOption.value ?? "")
										}
									/>
								</FormField>
								{dlq && (
									<FormField
										label="Maximum receives"
										constraintText="1–1000 receives before a message is moved to the dead-letter queue"
									>
										<Input
											ariaLabel="Maximum receives"
											value={count}
											type="number"
											onChange={({ detail }) => setCount(detail.value)}
										/>
									</FormField>
								)}
							</SpaceBetween>
						</Container>
						{!editing && (
							<Container header={<Header variant="h2">Tags — optional</Header>}>
								<FormField
									label="Tags"
									description='JSON object of string values, for example {"team":"platform"}.'
								>
									<Textarea
										ariaLabel="Queue tags"
										value={tags}
										rows={4}
										onChange={({ detail }) => setTags(detail.value)}
									/>
								</FormField>
							</Container>
						)}
						{editing && attributes?.Policy && (
							<Alert type="info">
								Edit the access policy on the Permissions tab.
							</Alert>
						)}
					</SpaceBetween>
				</fieldset>
			</Form>
		</ContentLayout>
	);
}
