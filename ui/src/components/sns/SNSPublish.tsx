import {
	Alert,
	Button,
	Container,
	FormField,
	Header,
	Hotspot,
	Input,
	RadioGroup,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { errorText, jsonObject } from "#/lib/consoleActions";
import { type SNSScope, topicCall } from "#/lib/sns";
import { HOTSPOT } from "#/tutorials/hotspots";

export function SNSPublish({
	scope,
	attributes,
	enabled,
	onPublished,
}: {
	scope: SNSScope;
	attributes: Record<string, string>;
	enabled: boolean;
	onPublished: () => void;
}) {
	const [message, setMessage] = useState("");
	const [subject, setSubject] = useState("");
	const [structure, setStructure] = useState("");
	const [messageAttributes, setMessageAttributes] = useState("{}");
	const [group, setGroup] = useState("");
	const [dedup, setDedup] = useState("");
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const [result, setResult] = useState("");
	const fifo = attributes.FifoTopic === "true";
	const publish = async () => {
		if (!enabled || busy) return;
		setBusy(true);
		setError("");
		setResult("");
		try {
			if (!message) throw new Error("Enter a message.");
			if (structure === "json")
				jsonObject(message, "Protocol-specific message");
			const input: Record<string, unknown> = {
				Message: message,
				MessageAttributes: jsonObject(messageAttributes, "Message attributes"),
			};
			if (subject) input.Subject = subject;
			if (structure) input.MessageStructure = structure;
			if (fifo) {
				if (!group) throw new Error("Enter a message group ID.");
				if (!dedup && attributes.ContentBasedDeduplication !== "true")
					throw new Error(
						"Enter a deduplication ID or enable content-based deduplication.",
					);
				input.MessageGroupId = group;
				if (dedup) input.MessageDeduplicationId = dedup;
			}
			const output = await topicCall<{
				MessageId: string;
				SequenceNumber?: string;
			}>(scope, "Publish", input);
			setResult(
				`Message accepted. Message ID: ${output.MessageId}${output.SequenceNumber ? `. Sequence number: ${output.SequenceNumber}` : ""}. Check deliveries for subscriber outcomes.`,
			);
			onPublished();
		} catch (err) {
			setError(errorText(err));
		} finally {
			setBusy(false);
		}
	};
	return (
		<Container header={<Header variant="h2">Publish message</Header>}>
			<SpaceBetween size="l">
				{!!error && (
					<Alert type="error" header="Publish failed">
						{error}
					</Alert>
				)}
				{!!result && (
					<Alert type="success" dismissible onDismiss={() => setResult("")}>
						{result}
					</Alert>
				)}
				<fieldset
					disabled={busy || !enabled}
					style={{ border: 0, margin: 0, padding: 0 }}
				>
					<SpaceBetween size="l">
						<FormField
							label="Subject — optional"
							constraintText="Maximum 100 characters on one line"
						>
							<Input
								ariaLabel="Message subject"
								value={subject}
								onChange={({ detail }) => setSubject(detail.value)}
							/>
						</FormField>
						<FormField label="Message structure">
							<RadioGroup
								value={structure}
								onChange={({ detail }) => setStructure(detail.value)}
								items={[
									{
										value: "",
										label: "Identical payload for all delivery protocols",
									},
									{
										value: "json",
										label: "Custom payload for each delivery protocol",
										description:
											'JSON object with a string "default" entry and optional protocol keys such as "sqs" or "lambda".',
									},
								]}
							/>
						</FormField>
						<FormField
							label="Message body"
							constraintText="Maximum 256 KiB, including attributes"
						>
							<Textarea
								ariaLabel="SNS message body"
								value={message}
								rows={10}
								onChange={({ detail }) => setMessage(detail.value)}
							/>
						</FormField>
						{fifo && (
							<>
								<FormField label="Message group ID">
									<Input
										ariaLabel="SNS message group ID"
										value={group}
										onChange={({ detail }) => setGroup(detail.value)}
									/>
								</FormField>
								<FormField
									label="Message deduplication ID"
									description={
										attributes.ContentBasedDeduplication === "true"
											? "Optional with content-based deduplication."
											: "Required for this topic."
									}
								>
									<Input
										ariaLabel="SNS message deduplication ID"
										value={dedup}
										onChange={({ detail }) => setDedup(detail.value)}
									/>
								</FormField>
							</>
						)}
						<FormField
							label="Message attributes — optional"
							description='JSON map, for example {"event":{"DataType":"String","StringValue":"order-created"}}. BinaryValue uses base64.'
						>
							<Textarea
								ariaLabel="SNS message attributes"
								value={messageAttributes}
								rows={5}
								onChange={({ detail }) => setMessageAttributes(detail.value)}
							/>
						</FormField>
					</SpaceBetween>
				</fieldset>
				<Hotspot hotspotId={HOTSPOT.snsPublish}>
					<Button
						variant="primary"
						disabled={!enabled}
						loading={busy}
						onClick={publish}
					>
						Publish message
					</Button>
				</Hotspot>
			</SpaceBetween>
		</Container>
	);
}
