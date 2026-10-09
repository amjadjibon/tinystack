import {
	Alert,
	Box,
	Button,
	Checkbox,
	Container,
	FormField,
	Header,
	Input,
	Link,
	Modal,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useEffect, useRef, useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import { errorText, integer } from "#/lib/consoleActions";
import { groupCall, type LogEvent, type LogsScope, logTime } from "#/lib/logs";

type Page = {
	Events?: LogEvent[];
	NextToken?: string;
	NextForwardToken?: string;
	NextBackwardToken?: string;
};
export function LogEvents({
	scope,
	enabled,
}: {
	scope: LogsScope;
	enabled: boolean;
}) {
	const [start, setStart] = useState(
			scope.stream ? "" : new Date(Date.now() - 3600000).toISOString(),
		),
		[end, setEnd] = useState(""),
		[filter, setFilter] = useState(""),
		[prefix, setPrefix] = useState(""),
		[limit, setLimit] = useState("100"),
		[latest, setLatest] = useState(true);
	const [page, setPage] = useState<Page>(),
		[busy, setBusy] = useState(false),
		[error, setError] = useState(""),
		[selected, setSelected] = useState<LogEvent>(),
		[done, setDone] = useState<"forward" | "backward" | undefined>();
	const request = useRef(0),
		locked = useRef(false),
		executed = useRef<
			{ operation: string; input: Record<string, unknown> } | undefined
		>(undefined);
	const load = async (
		params: { operation: string; input: Record<string, unknown> },
		token?: string,
		direction?: "forward" | "backward",
	) => {
		if (!enabled || locked.current) return;
		locked.current = true;
		setBusy(true);
		setError("");
		const id = ++request.current;
		try {
			const result = await groupCall<Page>(scope, params.operation, {
				...params.input,
				...(token ? { NextToken: token } : {}),
			});
			if (id !== request.current) return;
			setPage(result);
			setDone(
				token &&
					direction &&
					(direction === "forward"
						? result.NextForwardToken
						: result.NextBackwardToken) === token
					? direction
					: undefined,
			);
		} catch (e) {
			if (id === request.current) setError(errorText(e));
		} finally {
			if (id === request.current) {
				locked.current = false;
				setBusy(false);
			}
		}
	};
	const run = () => {
		if (locked.current || !enabled) return;
		try {
			const from = logTime(start, "Start time"),
				to = logTime(end, "End time");
			if (from !== undefined && to !== undefined && from > to)
				throw new Error("End time must be after start time.");
			const streaming = !!scope.stream && !filter.trim();
			const params = {
				operation: streaming ? "GetLogEvents" : "FilterLogEvents",
				input: {
					...(from !== undefined ? { StartTime: from } : {}),
					...(to !== undefined ? { EndTime: to } : {}),
					Limit: integer(limit, "Page size", 1, 10000),
					...(streaming
						? { LogStreamName: scope.stream, StartFromHead: !latest }
						: {
								...(scope.stream
									? { LogStreamNames: [scope.stream] }
									: prefix
										? { LogStreamNamePrefix: prefix }
										: {}),
								...(filter.trim() ? { FilterPattern: filter } : {}),
							}),
				},
			};
			executed.current = params;
			setPage(undefined);
			setDone(undefined);
			load(params);
		} catch (e) {
			setError(errorText(e));
		}
	};
	// Stream pages open at their latest events. Group searches stay explicit.
	// biome-ignore lint/correctness/useExhaustiveDependencies: the caller keys this reader by scope and server mode.
	useEffect(() => {
		if (scope.stream) run();
		return () => {
			request.current++;
			locked.current = false;
		};
	}, []);
	const filtered = executed.current?.operation === "FilterLogEvents";
	return (
		<SpaceBetween size="l">
			<Container
				header={
					<Header variant="h2">
						{scope.stream ? "Log events" : "Search log events"}
					</Header>
				}
			>
				<SpaceBetween size="l">
					{!enabled && (
						<Alert type="info">
							Use signed CLI/SDK requests to read log events when this server
							requires signatures.
						</Alert>
					)}
					{error && <Alert type="error">{error}</Alert>}
					<fieldset
						disabled={!enabled || busy}
						style={{ border: 0, padding: 0, margin: 0 }}
					>
						<SpaceBetween size="l">
							<SpaceBetween direction="horizontal" size="s">
								<FormField
									label="Start time — optional"
									description="ISO 8601, for example 2026-10-09T12:00:00Z."
								>
									<Input
										ariaLabel="Log start time"
										value={start}
										onChange={({ detail }) => setStart(detail.value)}
									/>
								</FormField>
								<FormField
									label="End time — optional"
									description="Exclusive. Leave blank for no upper bound."
								>
									<Input
										ariaLabel="Log end time"
										value={end}
										onChange={({ detail }) => setEnd(detail.value)}
									/>
								</FormField>
							</SpaceBetween>
							<FormField
								label="Filter pattern — optional"
								description='Terms and "quoted phrases", -excluded terms, and ?optional terms. JSON and regex filters are not supported.'
							>
								<Input
									ariaLabel="Log filter pattern"
									value={filter}
									onChange={({ detail }) => setFilter(detail.value)}
								/>
							</FormField>
							{!scope.stream && (
								<FormField label="Log stream prefix — optional">
									<Input
										ariaLabel="Log stream prefix"
										value={prefix}
										onChange={({ detail }) => setPrefix(detail.value)}
									/>
								</FormField>
							)}
							<FormField
								label="Events per page"
								constraintText="1–10,000, with a 1 MiB response limit."
							>
								<Input
									ariaLabel="Log page size"
									type="number"
									value={limit}
									onChange={({ detail }) => setLimit(detail.value)}
								/>
							</FormField>
							{scope.stream && (
								<Checkbox
									disabled={!!filter.trim()}
									checked={latest}
									onChange={({ detail }) => setLatest(detail.checked)}
								>
									Start with latest events (unfiltered reads)
								</Checkbox>
							)}
						</SpaceBetween>
					</fieldset>
					<Button
						variant="primary"
						disabled={!enabled}
						loading={busy}
						onClick={run}
					>
						{scope.stream ? "Refresh events" : "Search"}
					</Button>
				</SpaceBetween>
			</Container>
			<Table
				items={page?.Events ?? []}
				loading={busy}
				wrapLines
				resizableColumns
				header={
					<Header
						variant="h2"
						counter={`(${page?.Events?.length ?? 0})`}
						description="Paging uses the settings from the last search or refresh. Event reads are not polled."
					>
						Events
					</Header>
				}
				columnDefinitions={[
					{
						id: "time",
						header: "Timestamp",
						width: 200,
						cell: (e) => (
							<Link onFollow={() => setSelected(e)}>
								{e.Timestamp !== undefined
									? new Date(e.Timestamp).toLocaleString()
									: "—"}
							</Link>
						),
					},
					{
						id: "message",
						header: "Message",
						cell: (e) => (
							<span style={{ fontFamily: "monospace", whiteSpace: "pre-wrap" }}>
								{e.Message}
							</span>
						),
					},
					{
						id: "stream",
						header: "Log stream",
						cell: (e) => e.LogStreamName ?? scope.stream ?? "—",
					},
				]}
				empty={
					<Box textAlign="center" padding="l">
						{page
							? "No events on this page. Check for a continuation or change your search."
							: "Search to load log events."}
					</Box>
				}
				footer={
					<SpaceBetween direction="horizontal" size="xs">
						{!filtered && (
							<Button
								disabled={
									busy || !page?.NextBackwardToken || done === "backward"
								}
								onClick={() =>
									executed.current &&
									load(executed.current, page?.NextBackwardToken, "backward")
								}
							>
								Older events
							</Button>
						)}
						<Button
							disabled={
								busy ||
								!(filtered ? page?.NextToken : page?.NextForwardToken) ||
								done === "forward"
							}
							onClick={() =>
								executed.current &&
								load(
									executed.current,
									filtered ? page?.NextToken : page?.NextForwardToken,
									"forward",
								)
							}
						>
							{filtered ? "Next page" : "Newer events"}
						</Button>
						{done && (
							<Box>
								Reached the {done === "forward" ? "end" : "start"} of this
								stream. Refresh to read again.
							</Box>
						)}
					</SpaceBetween>
				}
			/>
			{selected && (
				<Modal
					visible
					size="large"
					header="Log event details"
					closeAriaLabel="Close log event"
					onDismiss={() => setSelected(undefined)}
					footer={
						<Box float="right">
							<Button onClick={() => setSelected(undefined)}>Close</Button>
						</Box>
					}
				>
					<SpaceBetween size="l">
						<Box>Stream: {selected.LogStreamName ?? scope.stream}</Box>
						<Box>
							Ingested:{" "}
							{selected.IngestionTime !== undefined
								? new Date(selected.IngestionTime).toLocaleString()
								: "—"}
						</Box>
						<CodeBlock text={selected.Message ?? ""} />
					</SpaceBetween>
				</Modal>
			)}
		</SpaceBetween>
	);
}
