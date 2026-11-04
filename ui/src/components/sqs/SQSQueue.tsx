import {
	Alert,
	Box,
	Button,
	Container,
	ContentLayout,
	CopyToClipboard,
	FormField,
	Header,
	KeyValuePairs,
	SpaceBetween,
	Tabs,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import type { Resource } from "#/lib/api";
import { type ActionStep, jsonObject, stringMap } from "#/lib/consoleActions";
import {
	type QueueAttributes,
	queueCall,
	queueNumbers,
	redriveTarget,
	type SQSScope,
	sqsCall,
} from "#/lib/sqs";
import { usePoll } from "#/lib/usePoll";
import { SQSMessages } from "./SQSMessages";
import { SQSQueueForm } from "./SQSQueueForm";
import { SQSRedrive } from "./SQSRedrive";

export function SQSQueue({
	scope,
	initialTab,
	queues,
	enabled,
	resource,
	onChanged,
	onDeleted,
}: {
	scope: SQSScope;
	/** initialTab opens a tab named in the URL (empty for the default). */
	initialTab: string;
	queues: Resource[];
	enabled: boolean;
	resource?: Resource;
	onChanged: (message: string) => void;
	onDeleted: () => void;
}) {
	const details = usePoll(async () =>
		enabled
			? await queueCall<{ Attributes?: QueueAttributes }>(
					scope,
					"GetQueueAttributes",
					{ AttributeNames: ["All"] },
				)
			: { Attributes: resource?.details as QueueAttributes | undefined },
	);
	const tags = usePoll(async () =>
		enabled
			? ((
					await queueCall<{ Tags?: Record<string, string> }>(
						scope,
						"ListQueueTags",
					)
				).Tags ?? {})
			: (resource?.tags ?? {}),
	);
	const url = usePoll(
		async () =>
			enabled
				? sqsCall<{ QueueUrl: string }>(scope, "GetQueueUrl", {
						QueueName: scope.queue,
					})
				: null,
		0,
	);
	const [editing, setEditing] = useState(false);
	const [dialog, setDialog] = useState<
		"delete" | "purge" | "policy" | "allow" | "tags" | null
	>(null);
	const [text, setText] = useState("");
	const [tab, setTab] = useState(initialTab || "configuration");
	const attributes =
		details.data?.Attributes ?? (resource?.details as QueueAttributes) ?? {};
	const arn =
		attributes.QueueArn ??
		resource?.arn ??
		`arn:aws:sqs:${scope.region}:${scope.account}:${scope.queue}`;
	const fifo = attributes.FifoQueue === "true";
	const refresh = (message: string) => {
		details.reload();
		tags.reload();
		onChanged(message);
	};
	if (editing)
		return (
			<SQSQueueForm
				scope={scope}
				queues={queues}
				attributes={attributes}
				onCancel={() => setEditing(false)}
				onSaved={() => {
					setEditing(false);
					refresh("Queue configuration updated.");
				}}
			/>
		);
	const plan = async (): Promise<ActionStep[]> => {
		if (dialog === "delete" || dialog === "purge")
			return [
				{
					label: dialog === "delete" ? "Delete queue" : "Purge queue",
					run: () =>
						queueCall(
							scope,
							dialog === "delete" ? "DeleteQueue" : "PurgeQueue",
						),
				},
			];
		if (dialog === "tags") {
			const next = stringMap(text, "Tags");
			const removed = Object.keys(tags.data ?? {}).filter((k) => !(k in next));
			// Remove first so replacing a full 50-tag set does not exceed the tag limit.
			return [
				...(removed.length
					? [
							{
								label: "Remove tags",
								run: () => queueCall(scope, "UntagQueue", { TagKeys: removed }),
							},
						]
					: []),
				...(Object.keys(next).length
					? [
							{
								label: "Save tags",
								run: () => queueCall(scope, "TagQueue", { Tags: next }),
							},
						]
					: []),
			];
		}
		const key = dialog === "policy" ? "Policy" : "RedriveAllowPolicy";
		if (text.trim())
			jsonObject(
				text,
				dialog === "policy" ? "Access policy" : "Redrive allow policy",
			);
		const value = text.trim();
		return [
			{
				label: "Save policy",
				run: () =>
					queueCall(scope, "SetQueueAttributes", {
						Attributes: { [key]: value },
					}),
			},
		];
	};
	const open = (mode: "policy" | "allow" | "tags") => {
		setText(
			mode === "tags"
				? JSON.stringify(tags.data ?? {}, null, 2)
				: (attributes[mode === "policy" ? "Policy" : "RedriveAllowPolicy"] ??
						""),
		);
		setDialog(mode);
	};
	const displayPolicy = (key: string) => {
		try {
			return JSON.stringify(JSON.parse(attributes[key] ?? "{}"), null, 2);
		} catch {
			return attributes[key] ?? "";
		}
	};
	return (
		<ContentLayout
			header={
				<Header
					variant="h1"
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button
								iconName="refresh"
								ariaLabel="Refresh queue"
								onClick={() => {
									details.reload();
									tags.reload();
									url.reload();
								}}
							/>
							<Button
								disabled={!enabled || !details.data?.Attributes}
								onClick={() => setEditing(true)}
							>
								Edit
							</Button>
							<Button disabled={!enabled} onClick={() => setDialog("purge")}>
								Purge
							</Button>
							<Button disabled={!enabled} onClick={() => setDialog("delete")}>
								Delete
							</Button>
							<Button
								variant="primary"
								disabled={!enabled}
								onClick={() => setTab("messages")}
							>
								Send and receive messages
							</Button>
						</SpaceBetween>
					}
				>
					{scope.queue}
				</Header>
			}
		>
			<SpaceBetween size="l">
				{!!(details.error || tags.error || url.error) && (
					<Alert type="error" header="Cannot load queue">
						{details.error || tags.error || url.error}
					</Alert>
				)}
				<Container header={<Header variant="h2">Details</Header>}>
					<KeyValuePairs
						columns={3}
						items={[
							{ label: "Name", value: scope.queue },
							{ label: "Type", value: fifo ? "FIFO" : "Standard" },
							{ label: "AWS Region", value: scope.region },
							{
								label: "ARN",
								value: (
									<CopyToClipboard
										variant="inline"
										textToCopy={arn}
										copyButtonAriaLabel="Copy queue ARN"
										copySuccessText="Copied"
										copyErrorText="Copy failed"
									/>
								),
							},
							{
								label: "URL",
								value: url.data?.QueueUrl ? (
									<CopyToClipboard
										variant="inline"
										textToCopy={url.data.QueueUrl}
										copyButtonAriaLabel="Copy queue URL"
										copySuccessText="Copied"
										copyErrorText="Copy failed"
									/>
								) : (
									"—"
								),
							},
							{
								label: "Created",
								value: attributes.CreatedTimestamp
									? new Date(
											Number(attributes.CreatedTimestamp) * 1000,
										).toLocaleString()
									: "—",
							},
							{
								label: "Messages available",
								value: attributes.ApproximateNumberOfMessages ?? "—",
							},
							{
								label: "Messages in flight",
								value: attributes.ApproximateNumberOfMessagesNotVisible ?? "—",
							},
							{
								label: "Messages delayed",
								value: attributes.ApproximateNumberOfMessagesDelayed ?? "—",
							},
						]}
					/>
				</Container>
				<Tabs
					activeTabId={tab}
					onChange={({ detail }) => setTab(detail.activeTabId)}
					tabs={[
						{
							id: "configuration",
							label: "Configuration",
							content: (
								<Container header={<Header variant="h2">Configuration</Header>}>
									<KeyValuePairs
										columns={2}
										items={[
											...queueNumbers.map((f) => ({
												label: f.label,
												value: `${attributes[f.key] ?? "—"} ${f.unit}`,
											})),
											{
												label: "Encryption",
												value: attributes.KmsMasterKeyId
													? `SSE-KMS (${attributes.KmsMasterKeyId})`
													: attributes.SqsManagedSseEnabled === "true"
														? "SSE-SQS"
														: "Disabled",
											},
											{
												label: "Dead-letter queue",
												value: redriveTarget(attributes) || "Disabled",
											},
											...(fifo
												? [
														{
															label: "Content-based deduplication",
															value:
																attributes.ContentBasedDeduplication === "true"
																	? "Enabled"
																	: "Disabled",
														},
														{
															label: "Deduplication scope",
															value: attributes.DeduplicationScope ?? "queue",
														},
														{
															label: "FIFO throughput limit",
															value:
																attributes.FifoThroughputLimit ?? "perQueue",
														},
													]
												: []),
										]}
									/>
								</Container>
							),
						},
						{
							id: "messages",
							label: "Send and receive messages",
							content: (
								<SQSMessages
									scope={scope}
									attributes={attributes}
									enabled={enabled}
									onChanged={refresh}
								/>
							),
						},
						{
							id: "permissions",
							label: "Permissions",
							content: (
								<SpaceBetween size="l">
									<Alert type="info">
										Queue access policies are stored but not evaluated.
										Identity-based IAM policies can be checked when
										authorization is enabled.
									</Alert>
									<Container
										header={
											<Header
												variant="h2"
												actions={
													<Button
														disabled={!enabled}
														onClick={() => open("policy")}
													>
														Edit
													</Button>
												}
											>
												Access policy
											</Header>
										}
									>
										<Textarea
											ariaLabel="Access policy"
											readOnly
											value={
												attributes.Policy
													? displayPolicy("Policy")
													: "No access policy configured."
											}
											rows={10}
										/>
									</Container>
									<Container
										header={
											<Header
												variant="h2"
												actions={
													<Button
														disabled={!enabled}
														onClick={() => open("allow")}
													>
														Edit
													</Button>
												}
											>
												Redrive allow policy
											</Header>
										}
									>
										<Textarea
											ariaLabel="Redrive allow policy"
											readOnly
											value={
												attributes.RedriveAllowPolicy
													? displayPolicy("RedriveAllowPolicy")
													: '{"redrivePermission":"allowAll"}'
											}
											rows={6}
										/>
									</Container>
								</SpaceBetween>
							),
						},
						{
							id: "redrive",
							label: "Dead-letter queue redrive",
							content: (
								<SQSRedrive
									scope={scope}
									queues={queues}
									arn={arn}
									fifo={fifo}
									enabled={enabled}
									onChanged={refresh}
								/>
							),
						},
						{
							id: "tags",
							label: "Tags",
							content: (
								<Container
									header={
										<Header
											variant="h2"
											counter={`(${Object.keys(tags.data ?? {}).length})`}
											actions={
												<Button
													disabled={!enabled || tags.loading || !!tags.error}
													onClick={() => open("tags")}
												>
													Manage tags
												</Button>
											}
										>
											Tags
										</Header>
									}
								>
									{Object.keys(tags.data ?? {}).length ? (
										<KeyValuePairs
											columns={2}
											items={Object.entries(tags.data ?? {}).map(
												([label, value]) => ({ label, value }),
											)}
										/>
									) : (
										<Box>No tags</Box>
									)}
								</Container>
							),
						},
					]}
				/>
			</SpaceBetween>
			{dialog && (
				<ActionDialog
					title={
						dialog === "delete"
							? `Delete queue ${scope.queue}`
							: dialog === "purge"
								? `Purge queue ${scope.queue}`
								: dialog === "tags"
									? "Manage tags"
									: dialog === "policy"
										? "Edit access policy"
										: "Edit redrive allow policy"
					}
					submit={
						dialog === "delete"
							? "Delete queue"
							: dialog === "purge"
								? "Purge queue"
								: "Save changes"
					}
					confirmation={
						dialog === "delete" || dialog === "purge" ? scope.queue : undefined
					}
					plan={plan}
					onClose={() => setDialog(null)}
					onSaved={() => {
						const mode = dialog;
						setDialog(null);
						if (mode === "delete") onDeleted();
						else
							refresh(
								mode === "purge" ? "Queue purged." : "Queue settings saved.",
							);
					}}
				>
					{dialog === "delete" || dialog === "purge" ? (
						<Alert type="warning">
							{dialog === "delete"
								? "Permanently delete the queue and all its messages."
								: "Permanently delete all messages, including delayed and in-flight messages. The queue and its configuration remain."}{" "}
							This cannot be undone.
						</Alert>
					) : (
						<FormField
							label={dialog === "tags" ? "Tags (JSON)" : "Policy (JSON)"}
							description={
								dialog === "tags"
									? 'Use an object of string values, for example {"team":"platform"}. Omit a key to remove it.'
									: dialog === "allow"
										? "Use redrivePermission: allowAll, denyAll, or byQueue with sourceQueueArns. Leave empty to allow all source queues."
										: "Enter an IAM policy document. Leave empty to remove the policy."
							}
						>
							<Textarea
								ariaLabel="Settings JSON"
								value={text}
								rows={14}
								onChange={({ detail }) => setText(detail.value)}
							/>
						</FormField>
					)}
				</ActionDialog>
			)}
		</ContentLayout>
	);
}
