import {
	Alert,
	Box,
	Button,
	Container,
	Header,
	KeyValuePairs,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useEffect, useRef, useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import {
	type GatewayScope,
	type GatewaySnapshot,
	gatewayCall,
} from "#/lib/apigateway";
import { errorText, stringMap } from "#/lib/consoleActions";
import { GatewayInput, GatewaySelect, GatewayText, options } from "./Fields";

type Response = {
	StatusCode: number;
	Headers: Record<string, string[]>;
	Body: string;
	IsBase64Encoded: boolean;
	BodyTruncated: boolean;
};
export function RequestTest({
	scope,
	snapshot,
	enabled,
}: {
	scope: GatewayScope;
	snapshot: GatewaySnapshot;
	enabled: boolean;
}) {
	const [stage, setStage] = useState(
			snapshot.stages.find((s) => s.StageName === "$default")?.StageName ??
				snapshot.stages[0]?.StageName ??
				"",
		),
		[method, setMethod] = useState("GET"),
		[path, setPath] = useState("/"),
		[headers, setHeaders] = useState("{}"),
		[body, setBody] = useState("");
	const [response, setResponse] = useState<Response>(),
		[error, setError] = useState(""),
		[busy, setBusy] = useState(false),
		[sent, setSent] = useState(""),
		[duration, setDuration] = useState(0);
	const generation = useRef(0),
		locked = useRef(false);
	useEffect(
		() => () => {
			generation.current++;
			locked.current = false;
		},
		[],
	);
	const send = async () => {
		if (locked.current || !enabled) return;
		locked.current = true;
		setBusy(true);
		setError("");
		setResponse(undefined);
		const id = ++generation.current,
			started = performance.now();
		try {
			if (!stage) throw new Error("Choose a deployed stage.");
			if (!path.startsWith("/") || path.startsWith("//") || path.includes("#"))
				throw new Error(
					"Enter a path beginning with / and an optional query string.",
				);
			const result = await gatewayCall<Response>(scope, "InvokeApi", {
				ApiId: scope.apiId,
				Stage: stage,
				Method: method,
				Path: path,
				Headers: stringMap(headers, "Headers"),
				Body: body,
			});
			if (id === generation.current) {
				setResponse(result);
				setSent(`${method} ${path} · ${stage}`);
				setDuration(Math.round(performance.now() - started));
			}
		} catch (err) {
			if (id === generation.current) setError(errorText(err));
		} finally {
			if (id === generation.current) {
				setBusy(false);
				locked.current = false;
			}
		}
	};
	return (
		<SpaceBetween size="l">
			<Container header={<Header variant="h2">Test deployed API</Header>}>
				<SpaceBetween size="l">
					<Alert type="warning">
						Sending a request executes the deployed route, authorizers and
						integration. It can invoke Lambda or an HTTP backend and produce
						side effects. AWS_IAM routes require a signed execute-api request
						through the CLI/SDK.
					</Alert>
					<GatewaySelect
						label="Stage"
						value={stage}
						onChange={setStage}
						options={snapshot.stages.map((s) => ({
							value: s.StageName,
							label: s.StageName,
						}))}
					/>
					<GatewaySelect
						label="Request method"
						value={method}
						onChange={setMethod}
						options={options([
							"GET",
							"POST",
							"PUT",
							"PATCH",
							"DELETE",
							"HEAD",
							"OPTIONS",
						])}
					/>
					<GatewayInput
						label="Request path"
						value={path}
						onChange={setPath}
						description="Exclude the stage prefix; use a concrete path such as /pets/7?expand=true."
					/>
					<GatewayText
						label="Request headers (JSON)"
						value={headers}
						onChange={setHeaders}
						description='String values, for example {"Content-Type":"application/json"}. Bearer authorizers can use an Authorization header.'
					/>
					<GatewayText
						label="Request body"
						value={body}
						onChange={setBody}
						rows={8}
					/>
					{error && (
						<Alert type="error" header="Request failed">
							{error}
						</Alert>
					)}
					<Box>
						<Button
							variant="primary"
							loading={busy}
							disabled={!enabled || !stage}
							onClick={() => void send()}
						>
							Send request
						</Button>
					</Box>
				</SpaceBetween>
			</Container>
			{response && (
				<Container header={<Header variant="h2">Response</Header>}>
					<SpaceBetween size="m">
						<KeyValuePairs
							columns={3}
							items={[
								{ label: "Request", value: sent },
								{ label: "Status", value: response.StatusCode },
								{ label: "Duration", value: `${duration} ms` },
							]}
						/>
						{response.BodyTruncated && (
							<Alert type="warning">Response body truncated to 1 MiB.</Alert>
						)}
						{response.IsBase64Encoded && (
							<Alert type="info">
								The response body is displayed as base64.
							</Alert>
						)}
						<Header variant="h3">Headers</Header>
						<CodeBlock text={JSON.stringify(response.Headers, null, 2)} />
						<Header variant="h3">Body</Header>
						<CodeBlock text={response.Body} />
					</SpaceBetween>
				</Container>
			)}
		</SpaceBetween>
	);
}
