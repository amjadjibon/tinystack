import {
	Alert,
	Box,
	Button,
	FormField,
	Input,
	Modal,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { errorText, jsonObject } from "#/lib/consoleActions";
import { type EventScope, eventCall } from "#/lib/eventbridge";
export function EventPublish({
	scope,
	onClose,
}: {
	scope: EventScope;
	onClose: () => void;
}) {
	const [source, setSource] = useState("my.app"),
		[kind, setKind] = useState("Example"),
		[detail, setDetail] = useState("{}"),
		[resources, setResources] = useState("");
	const [busy, setBusy] = useState(false),
		[error, setError] = useState(""),
		[id, setId] = useState("");
	const locked = useRef(false);
	const publish = async () => {
		if (locked.current) return;
		locked.current = true;
		setBusy(true);
		setError("");
		setId("");
		try {
			if (!source.trim() || !kind.trim())
				throw new Error("Enter a source and detail type.");
			jsonObject(detail, "Event detail");
			const result = await eventCall<{
				Entries?: {
					EventId?: string;
					ErrorCode?: string;
					ErrorMessage?: string;
				}[];
			}>(scope, "PutEvents", {
				Entries: [
					{
						EventBusName: scope.bus || "default",
						Source: source,
						DetailType: kind,
						Detail: detail,
						Resources: resources
							.split("\n")
							.map((s) => s.trim())
							.filter(Boolean),
					},
				],
			});
			const entry = result.Entries?.[0];
			if (!entry?.EventId)
				throw new Error(
					`${entry?.ErrorCode ?? "Publication failed"}: ${entry?.ErrorMessage ?? "No event ID returned."}`,
				);
			setId(entry.EventId);
		} catch (e) {
			setError(errorText(e));
		} finally {
			locked.current = false;
			setBusy(false);
		}
	};
	return (
		<Modal
			visible
			size="large"
			header="Send event"
			closeAriaLabel="Close send event"
			onDismiss={() => !busy && onClose()}
			footer={
				<Box float="right">
					<SpaceBetween direction="horizontal" size="xs">
						<Button disabled={busy} onClick={onClose}>
							Close
						</Button>
						<Button variant="primary" loading={busy} onClick={publish}>
							Send event
						</Button>
					</SpaceBetween>
				</Box>
			}
		>
			<SpaceBetween size="l">
				<Alert type="info">
					Send to {scope.bus || "default"}. Matching enabled rules deliver this
					event to their targets. Check delivery history for outcomes.
				</Alert>
				{error && <Alert type="error">{error}</Alert>}
				{id && <Alert type="success">Event accepted. Event ID: {id}</Alert>}
				<fieldset disabled={busy} style={{ border: 0, margin: 0, padding: 0 }}>
					<SpaceBetween size="l">
						<FormField label="Event source">
							<Input
								ariaLabel="Event source"
								value={source}
								onChange={({ detail }) => setSource(detail.value)}
							/>
						</FormField>
						<FormField label="Detail type">
							<Input
								ariaLabel="Detail type"
								value={kind}
								onChange={({ detail }) => setKind(detail.value)}
							/>
						</FormField>
						<FormField label="Event detail (JSON)">
							<Textarea
								ariaLabel="Event detail"
								rows={8}
								value={detail}
								onChange={({ detail }) => setDetail(detail.value)}
							/>
						</FormField>
						<FormField
							label="Resource ARNs — optional"
							description="One ARN per line."
						>
							<Textarea
								ariaLabel="Event resources"
								value={resources}
								onChange={({ detail }) => setResources(detail.value)}
							/>
						</FormField>
					</SpaceBetween>
				</fieldset>
			</SpaceBetween>
		</Modal>
	);
}
