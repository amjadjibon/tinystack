import {
	Alert,
	Button,
	Container,
	FormField,
	Header,
	Hotspot,
	Input,
	KeyValuePairs,
	RadioGroup,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import { pretty } from "#/lib/api";
import { errorText } from "#/lib/consoleActions";
import {
	decodeText,
	encodeBytes,
	functionCall,
	type LambdaScope,
} from "#/lib/lambda";
import { HOTSPOT } from "#/tutorials/hotspots";

type Invocation = {
	StatusCode?: number;
	FunctionError?: string;
	Payload?: string;
	LogResult?: string;
	ExecutedVersion?: string;
};
export function LambdaTest({
	scope,
	enabled,
	onInvoked,
}: {
	scope: LambdaScope;
	enabled: boolean;
	onInvoked: () => void;
}) {
	const [payload, setPayload] = useState(
		'{\n  "message": "Hello from the console"\n}',
	);
	const [mode, setMode] = useState("RequestResponse");
	const [qualifier, setQualifier] = useState("");
	const [result, setResult] = useState<Invocation>();
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const invoke = async () => {
		if (busy || !enabled) return;
		setBusy(true);
		setError("");
		setResult(undefined);
		try {
			JSON.parse(payload);
			const bytes = new TextEncoder().encode(payload);
			if (bytes.length > 1024 * 1024)
				throw new Error("Console test events are limited to 1 MiB.");
			setResult(
				await functionCall<Invocation>(scope, "Invoke", {
					Payload: encodeBytes(bytes),
					InvocationType: mode,
					...(mode === "RequestResponse" ? { LogType: "Tail" } : {}),
					...(qualifier.trim() ? { Qualifier: qualifier.trim() } : {}),
				}),
			);
			onInvoked();
		} catch (err) {
			setError(errorText(err));
		} finally {
			setBusy(false);
		}
	};
	return (
		<SpaceBetween size="l">
			<Container header={<Header variant="h2">Test event</Header>}>
				<SpaceBetween size="l">
					<Alert type="info">
						Invoking runs your deployed code and can change resources. Docker
						must be available on the TinyStack host.
					</Alert>
					{!!error && (
						<Alert type="error" header="Invocation failed">
							{error}
						</Alert>
					)}
					<fieldset
						disabled={!enabled || busy}
						style={{ border: 0, margin: 0, padding: 0 }}
					>
						<SpaceBetween size="l">
							<FormField label="Invocation type">
								<RadioGroup
									value={mode}
									onChange={({ detail }) => setMode(detail.value)}
									items={[
										{
											value: "RequestResponse",
											label: "Synchronous",
											description:
												"Wait for the response and the last 4 KiB of logs.",
										},
										{
											value: "Event",
											label: "Asynchronous",
											description:
												"Queue the event. Inspect logs for the execution result.",
										},
										{
											value: "DryRun",
											label: "Dry run",
											description:
												"Validate that the function or qualifier exists without executing code.",
										},
									]}
								/>
							</FormField>
							<FormField
								label="Version or alias — optional"
								description="Leave empty to invoke $LATEST."
							>
								<Input
									ariaLabel="Invocation qualifier"
									value={qualifier}
									onChange={({ detail }) => setQualifier(detail.value)}
								/>
							</FormField>
							<FormField
								label="Event JSON"
								constraintText="Maximum 1 MiB in the console."
							>
								<Textarea
									ariaLabel="Test event JSON"
									value={payload}
									rows={12}
									onChange={({ detail }) => setPayload(detail.value)}
								/>
							</FormField>
						</SpaceBetween>
					</fieldset>
					<Hotspot hotspotId={HOTSPOT.lambdaTest}>
						<Button
							variant="primary"
							disabled={!enabled}
							loading={busy}
							onClick={invoke}
						>
							Test
						</Button>
					</Hotspot>
				</SpaceBetween>
			</Container>
			{result && (
				<Container header={<Header variant="h2">Execution result</Header>}>
					<SpaceBetween size="l">
						<Alert
							type={result.FunctionError ? "error" : "success"}
							header={
								result.FunctionError
									? "Function returned an error"
									: result.StatusCode === 202
										? "Event accepted"
										: result.StatusCode === 204
											? "Dry run succeeded"
											: "Execution succeeded"
							}
						>
							{result.StatusCode === 202
								? "The event is queued. Acceptance does not confirm successful execution; open Monitor to inspect logs."
								: result.FunctionError
									? result.FunctionError
									: ""}
						</Alert>
						<KeyValuePairs
							columns={2}
							items={[
								{ label: "Status code", value: result.StatusCode },
								{
									label: "Executed version",
									value: result.ExecutedVersion ?? "—",
								},
							]}
						/>
						{!!result.Payload && (
							<SpaceBetween size="l">
								<Header variant="h3">Response</Header>
								<CodeBlock text={pretty(decodeText(result.Payload))} />
							</SpaceBetween>
						)}
						{!!result.LogResult && (
							<SpaceBetween size="l">
								<Header variant="h3">Log output (last 4 KiB)</Header>
								<CodeBlock text={decodeText(result.LogResult)} />
							</SpaceBetween>
						)}
					</SpaceBetween>
				</Container>
			)}
		</SpaceBetween>
	);
}
