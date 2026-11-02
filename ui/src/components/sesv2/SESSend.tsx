import {
	Alert,
	Box,
	Button,
	Container,
	FormField,
	Header,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { Pick, Text } from "#/components/memorydb/Fields";
import { errorText } from "#/lib/consoleActions";
import {
	type IdentityInfo,
	type SESScope,
	sesv2Call,
	type TemplateMetadata,
} from "#/lib/sesv2";
import {
	emptySend,
	type SendForm,
	senderOptions,
	sendInput,
} from "#/lib/sesv2Helpers";

const none = "(none)";

export function SESSend({
	scope,
	enabled,
	identities,
	templates,
	configSets,
	onSent,
}: {
	scope: SESScope;
	enabled: boolean;
	identities: IdentityInfo[];
	templates: TemplateMetadata[];
	configSets: string[];
	onSent: () => void;
}) {
	const [form, setForm] = useState<SendForm>(emptySend),
		[busy, setBusy] = useState(false),
		[error, setError] = useState(""),
		[messageId, setMessageId] = useState("");
	const patch = (change: Partial<SendForm>) => setForm({ ...form, ...change });
	const senders = senderOptions(identities);
	const send = async () => {
		setBusy(true);
		setError("");
		setMessageId("");
		try {
			const out = await sesv2Call<{ MessageId?: string }>(
				scope,
				"SendEmail",
				sendInput(form),
			);
			setMessageId(out.MessageId ?? "");
			onSent();
		} catch (err) {
			setError(errorText(err));
		} finally {
			setBusy(false);
		}
	};
	return (
		<Container
			header={
				<Header
					variant="h2"
					description="TinyStack records the message instead of delivering it. Open Sent messages to read it."
					actions={
						<Button
							variant="primary"
							loading={busy}
							disabled={!enabled}
							onClick={send}
						>
							Send test email
						</Button>
					}
				>
					Send test email
				</Header>
			}
		>
			<SpaceBetween size="m">
				{error && (
					<Alert type="error" header="Send failed">
						{error}
					</Alert>
				)}
				{messageId && (
					<Alert type="success" header="Message accepted">
						Message ID {messageId}
					</Alert>
				)}
				<Pick
					label="From"
					value={form.from}
					placeholder="Choose a verified sender"
					description={
						senders.length
							? "Email identities send as themselves; a domain identity sends from a no-reply address."
							: "Create an identity on the Identities tab first."
					}
					options={senders}
					onChange={(from) => patch({ from })}
				/>
				<Text
					label="To"
					value={form.to}
					onChange={(to) => patch({ to })}
					description="Separate addresses with commas."
				/>
				<Text label="Cc" value={form.cc} onChange={(cc) => patch({ cc })} />
				<Text label="Bcc" value={form.bcc} onChange={(bcc) => patch({ bcc })} />
				<Text
					label="Reply-to"
					value={form.replyTo}
					onChange={(replyTo) => patch({ replyTo })}
				/>
				<Pick
					label="Configuration set (optional)"
					value={form.configurationSet || none}
					placeholder="Choose a configuration set"
					options={[{ value: none }, ...configSets.map((value) => ({ value }))]}
					onChange={(v) => patch({ configurationSet: v === none ? "" : v })}
				/>
				<Pick
					label="Content"
					value={form.mode}
					placeholder="Choose the content type"
					options={[
						{ value: "simple", label: "Simple message" },
						{ value: "template", label: "Stored template" },
					]}
					onChange={(mode) => patch({ mode: mode as SendForm["mode"] })}
				/>
				{form.mode === "simple" ? (
					<>
						<Text
							label="Subject"
							value={form.subject}
							onChange={(subject) => patch({ subject })}
						/>
						<FormField label="Text body">
							<Textarea
								ariaLabel="Text body"
								rows={5}
								value={form.text}
								onChange={({ detail }) => patch({ text: detail.value })}
							/>
						</FormField>
						<FormField
							label="HTML body"
							description="At least one of the text and HTML bodies is required."
						>
							<Textarea
								ariaLabel="HTML body"
								rows={5}
								value={form.html}
								onChange={({ detail }) => patch({ html: detail.value })}
							/>
						</FormField>
					</>
				) : (
					<>
						<Pick
							label="Template"
							value={form.template}
							placeholder="Choose a template"
							description={
								templates.length
									? undefined
									: "Create a template on the Templates tab first."
							}
							options={templates.map((t) => ({ value: t.TemplateName ?? "" }))}
							onChange={(template) => patch({ template })}
						/>
						<FormField
							label="Template data (JSON)"
							description="Values for the placeholders, as a JSON object."
						>
							<Textarea
								ariaLabel="Template data"
								rows={5}
								value={form.data}
								onChange={({ detail }) => patch({ data: detail.value })}
							/>
						</FormField>
					</>
				)}
				<Box color="text-body-secondary" fontSize="body-s">
					Quotas are reported but not enforced.
				</Box>
			</SpaceBetween>
		</Container>
	);
}
