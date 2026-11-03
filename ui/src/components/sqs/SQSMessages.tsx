import {
	Alert,
	Box,
	Button,
	Container,
	CopyToClipboard,
	FormField,
	Header,
	Hotspot,
	Input,
	KeyValuePairs,
	Link,
	Modal,
	SpaceBetween,
	Table,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { pretty } from "#/lib/api";
import { errorText, integer, jsonObject } from "#/lib/consoleActions";
import {
	type QueueAttributes,
	queueCall,
	type SQSMessage,
	type SQSScope,
} from "#/lib/sqs";
import { HOTSPOT } from "#/tutorials/hotspots";

export function SQSMessages({
	scope,
	attributes,
	enabled,
	onChanged,
}: {
	scope: SQSScope;
	attributes: QueueAttributes;
	enabled: boolean;
	onChanged: (message: string) => void;
}) {
	const fifo = attributes.FifoQueue === "true";
	const [body, setBody] = useState("");
	const [messageAttributes, setMessageAttributes] = useState("{}");
	const [group, setGroup] = useState("");
	const [dedup, setDedup] = useState("");
	const [delay, setDelay] = useState("0");
	const [max, setMax] = useState("10");
	const [wait, setWait] = useState("0");
	const [visibility, setVisibility] = useState(
		attributes.VisibilityTimeout ?? "30",
	);
	const [messages, setMessages] = useState<SQSMessage[]>([]);
	const [selected, setSelected] = useState<SQSMessage[]>([]);
	const [inspected, setInspected] = useState<SQSMessage | null>(null);
	const [busy, setBusy] = useState<"send" | "receive" | null>(null);
	const [error, setError] = useState("");
	const [result, setResult] = useState("");
	const [received, setReceived] = useState(false);
	const [dialog, setDialog] = useState<"delete" | "visibility" | null>(null);
	const [timeout, setTimeoutValue] = useState("0");
	const target = selected[0];
	const send = async () => {
		if (busy || !enabled) return;
		setBusy("send");
		setError("");
		setResult("");
		try {
			if (!body) throw new Error("Enter a message body.");
			const input: Record<string, unknown> = {
				MessageBody: body,
				MessageAttributes: jsonObject(messageAttributes, "Message attributes"),
			};
			if (fifo) {
				if (!group)
					throw new Error("FIFO messages require a message group ID.");
				if (!dedup && attributes.ContentBasedDeduplication !== "true")
					throw new Error(
						"Enter a deduplication ID or enable content-based deduplication.",
					);
				input.MessageGroupId = group;
				if (dedup) input.MessageDeduplicationId = dedup;
			} else input.DelaySeconds = integer(delay, "Delivery delay", 0, 900);
			const output = await queueCall<{
				MessageId: string;
				SequenceNumber?: string;
			}>(scope, "SendMessage", input);
			setResult(
				`Message sent. Message ID: ${output.MessageId}${output.SequenceNumber ? `. Sequence number: ${output.SequenceNumber}` : ""}`,
			);
			onChanged("Message sent.");
		} catch (err) {
			setError(errorText(err));
		} finally {
			setBusy(null);
		}
	};
	const receive = async () => {
		if (busy || !enabled) return;
		setBusy("receive");
		setError("");
		try {
			const output = await queueCall<{ Messages?: SQSMessage[] }>(
				scope,
				"ReceiveMessage",
				{
					MaxNumberOfMessages: integer(max, "Maximum messages", 1, 10),
					WaitTimeSeconds: integer(wait, "Wait time", 0, 20),
					VisibilityTimeout: integer(
						visibility,
						"Visibility timeout",
						0,
						43200,
					),
					MessageSystemAttributeNames: ["All"],
					MessageAttributeNames: ["All"],
				},
			);
			setMessages(output.Messages ?? []);
			setSelected([]);
			setReceived(true);
			onChanged("Message poll completed.");
		} catch (err) {
			setError(errorText(err));
		} finally {
			setBusy(null);
		}
	};
	return (
		<SpaceBetween size="l">
			{!!error && (
				<Alert type="error" header="Message action failed">
					{error}
				</Alert>
			)}
			{!!result && (
				<Alert type="success" dismissible onDismiss={() => setResult("")}>
					{result}
				</Alert>
			)}
			<Container header={<Header variant="h2">Send message</Header>}>
				<SpaceBetween size="l">
					<FormField
						label="Message body"
						constraintText={`Queue maximum: ${attributes.MaximumMessageSize ?? "262144"} bytes, including attributes.`}
					>
						<Textarea
							ariaLabel="Message body"
							value={body}
							rows={6}
							disabled={!!busy || !enabled}
							onChange={({ detail }) => setBody(detail.value)}
						/>
					</FormField>
					{fifo ? (
						<SpaceBetween direction="horizontal" size="l">
							<FormField label="Message group ID">
								<Input
									ariaLabel="Message group ID"
									value={group}
									disabled={!!busy || !enabled}
									onChange={({ detail }) => setGroup(detail.value)}
								/>
							</FormField>
							<FormField
								label="Message deduplication ID"
								description={
									attributes.ContentBasedDeduplication === "true"
										? "Optional with content-based deduplication."
										: "Required for this queue."
								}
							>
								<Input
									ariaLabel="Message deduplication ID"
									value={dedup}
									disabled={!!busy || !enabled}
									onChange={({ detail }) => setDedup(detail.value)}
								/>
							</FormField>
						</SpaceBetween>
					) : (
						<FormField label="Delivery delay" constraintText="0–900 seconds">
							<Input
								ariaLabel="Message delivery delay"
								type="number"
								value={delay}
								disabled={!!busy || !enabled}
								onChange={({ detail }) => setDelay(detail.value)}
							/>
						</FormField>
					)}
					<FormField
						label="Message attributes — optional"
						description='JSON map, for example {"priority":{"DataType":"Number","StringValue":"1"}}. BinaryValue uses base64.'
					>
						<Textarea
							ariaLabel="Message attributes"
							value={messageAttributes}
							rows={3}
							disabled={!!busy || !enabled}
							onChange={({ detail }) => setMessageAttributes(detail.value)}
						/>
					</FormField>
					<Button
						variant="primary"
						disabled={!enabled || !!busy}
						loading={busy === "send"}
						onClick={send}
					>
						Send message
					</Button>
				</SpaceBetween>
			</Container>
			<Container header={<Header variant="h2">Receive messages</Header>}>
				<SpaceBetween size="l">
					<Alert type="info">
						Polling receives messages and hides them for the visibility timeout.
						Each receive increases the receive count and can move messages to a
						dead-letter queue. Messages return to the queue unless deleted.
						Polling again replaces the results below.
					</Alert>
					<SpaceBetween direction="horizontal" size="l">
						<FormField label="Maximum messages" constraintText="1–10">
							<Input
								ariaLabel="Maximum messages"
								type="number"
								value={max}
								disabled={!!busy || !enabled}
								onChange={({ detail }) => setMax(detail.value)}
							/>
						</FormField>
						<FormField label="Wait time" constraintText="0–20 seconds">
							<Input
								ariaLabel="Poll wait time"
								type="number"
								value={wait}
								disabled={!!busy || !enabled}
								onChange={({ detail }) => setWait(detail.value)}
							/>
						</FormField>
						<FormField
							label="Visibility timeout"
							constraintText="0–43200 seconds"
						>
							<Input
								ariaLabel="Receive visibility timeout"
								type="number"
								value={visibility}
								disabled={!!busy || !enabled}
								onChange={({ detail }) => setVisibility(detail.value)}
							/>
						</FormField>
					</SpaceBetween>
					<Hotspot hotspotId={HOTSPOT.sqsReceive}>
						<Button
							variant="primary"
							disabled={!enabled || !!busy}
							loading={busy === "receive"}
							onClick={receive}
						>
							Poll for messages
						</Button>
					</Hotspot>
				</SpaceBetween>
			</Container>
			<Table
				items={messages}
				trackBy="ReceiptHandle"
				selectionType="single"
				selectedItems={selected}
				onSelectionChange={({ detail }) => setSelected(detail.selectedItems)}
				ariaLabels={{
					selectionGroupLabel: "Select received message",
					itemSelectionLabel: (_, m) => `Select message ${m.MessageId}`,
				}}
				header={
					<Header
						variant="h2"
						counter={`(${messages.length})`}
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									disabled={!target || !enabled || !!busy}
									onClick={() => {
										setTimeoutValue("0");
										setDialog("visibility");
									}}
								>
									Change visibility
								</Button>
								<Button
									disabled={!target || !enabled || !!busy}
									onClick={() => setDialog("delete")}
								>
									Delete message
								</Button>
							</SpaceBetween>
						}
					>
						Received messages
					</Header>
				}
				columnDefinitions={[
					{
						id: "id",
						header: "Message ID",
						cell: (m) => (
							<Link onFollow={() => setInspected(m)}>{m.MessageId}</Link>
						),
					},
					{
						id: "body",
						header: "Message body",
						cell: (m) =>
							m.Body.length > 100 ? `${m.Body.slice(0, 100)}…` : m.Body,
					},
					{
						id: "count",
						header: "Receive count",
						cell: (m) => m.Attributes?.ApproximateReceiveCount ?? "—",
					},
					{
						id: "group",
						header: "Message group ID",
						cell: (m) => m.Attributes?.MessageGroupId ?? "—",
					},
				]}
				empty={
					<Box textAlign="center" padding="l">
						{received
							? "No messages returned. Messages may be delayed, in flight, or the queue may be empty."
							: "Poll for messages to inspect them."}
					</Box>
				}
			/>
			{inspected && (
				<Modal
					visible
					size="large"
					header="Message details"
					onDismiss={() => setInspected(null)}
					closeAriaLabel="Close message details"
					footer={
						<Box float="right">
							<Button onClick={() => setInspected(null)}>Close</Button>
						</Box>
					}
				>
					<SpaceBetween size="l">
						<KeyValuePairs
							columns={2}
							items={[
								{ label: "Message ID", value: inspected.MessageId },
								{ label: "Body MD5", value: inspected.MD5OfBody },
								{
									label: "Receipt handle",
									value: (
										<CopyToClipboard
											copyButtonText="Copy receipt handle"
											copySuccessText="Copied"
											copyErrorText="Copy failed"
											textToCopy={inspected.ReceiptHandle}
										/>
									),
								},
							]}
						/>
						<FormField label="Body">
							<Textarea
								ariaLabel="Received message body"
								readOnly
								value={pretty(inspected.Body)}
								rows={8}
							/>
						</FormField>
						<FormField label="System attributes">
							<Textarea
								readOnly
								value={JSON.stringify(inspected.Attributes ?? {}, null, 2)}
								rows={5}
							/>
						</FormField>
						<FormField label="Message attributes">
							<Textarea
								readOnly
								value={JSON.stringify(
									inspected.MessageAttributes ?? {},
									null,
									2,
								)}
								rows={5}
							/>
						</FormField>
					</SpaceBetween>
				</Modal>
			)}
			{dialog && target && (
				<ActionDialog
					title={
						dialog === "delete" ? "Delete message" : "Change message visibility"
					}
					submit={dialog === "delete" ? "Delete message" : "Change visibility"}
					onClose={() => setDialog(null)}
					onSaved={() => {
						setMessages((previous) =>
							previous.filter((m) => m.ReceiptHandle !== target.ReceiptHandle),
						);
						setSelected([]);
						setDialog(null);
						onChanged(
							dialog === "delete"
								? "Message deleted."
								: "Message visibility changed. Poll again to receive it with a new receipt handle.",
						);
					}}
					plan={async () => {
						const seconds =
							dialog === "visibility"
								? integer(timeout, "Visibility timeout", 0, 43200)
								: 0;
						return [
							{
								label:
									dialog === "delete" ? "Delete message" : "Change visibility",
								run: () =>
									queueCall(
										scope,
										dialog === "delete"
											? "DeleteMessage"
											: "ChangeMessageVisibility",
										{
											ReceiptHandle: target.ReceiptHandle,
											...(dialog === "visibility"
												? { VisibilityTimeout: seconds }
												: {}),
										},
									),
							},
						];
					}}
				>
					<SpaceBetween size="l">
						<Box>Message ID: {target.MessageId}</Box>
						{dialog === "delete" ? (
							<Alert type="warning">
								Permanently delete this message using its receipt handle. This
								cannot be undone.
							</Alert>
						) : (
							<FormField
								label="Visibility timeout"
								description="0 makes this message available immediately. Receipt handles can expire; poll again if the API rejects an old handle."
								constraintText="0–43200 seconds"
							>
								<Input
									ariaLabel="New visibility timeout"
									value={timeout}
									type="number"
									onChange={({ detail }) => setTimeoutValue(detail.value)}
								/>
							</FormField>
						)}
					</SpaceBetween>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
