import {
	Alert,
	Box,
	Button,
	Checkbox,
	Container,
	FormField,
	Header,
	Input,
	KeyValuePairs,
	Modal,
	SpaceBetween,
	StatusIndicator,
	Table,
	Tabs,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { pretty } from "#/lib/api";
import {
	type Execution,
	eventDetails,
	eventState,
	executionCall,
	type HistoryEvent,
	type SfnScope,
	timestamp,
	useSfnPage,
} from "#/lib/sfn";
import { usePoll } from "#/lib/usePoll";

export function ExecutionStatus({ status }: { status?: string }) {
	return (
		<StatusIndicator
			type={
				status === "SUCCEEDED"
					? "success"
					: status === "RUNNING"
						? "in-progress"
						: status === "FAILED" || status === "TIMED_OUT"
							? "error"
							: "stopped"
			}
		>
			{status ?? "Unknown"}
		</StatusIndicator>
	);
}
export function ExecutionResult({ execution }: { execution: Execution }) {
	return (
		<SpaceBetween size="l">
			<Container header={<Header variant="h2">Execution details</Header>}>
				<KeyValuePairs
					columns={3}
					items={[
						{
							label: "Status",
							value: <ExecutionStatus status={execution.Status} />,
						},
						{ label: "Execution ARN", value: execution.ExecutionArn },
						{ label: "Started", value: timestamp(execution.StartDate) },
						{ label: "Finished", value: timestamp(execution.StopDate) },
						{
							label: "Duration",
							value:
								execution.StopDate === undefined
									? "In progress"
									: `${Math.max(0, execution.StopDate - execution.StartDate).toFixed(3)} seconds`,
						},
						...(execution.StateMachineVersionArn
							? [
									{
										label: "State machine version",
										value: execution.StateMachineVersionArn,
									},
								]
							: []),
						...(execution.StateMachineAliasArn
							? [
									{
										label: "State machine alias",
										value: execution.StateMachineAliasArn,
									},
								]
							: []),
					]}
				/>
			</Container>
			{(execution.Error || execution.Cause) && (
				<Alert type="error" header={execution.Error ?? "Execution stopped"}>
					<CodeBlock text={execution.Cause ?? ""} />
				</Alert>
			)}
			<Container header={<Header variant="h2">Input</Header>}>
				<CodeBlock
					text={
						execution.Input === undefined
							? "Input unavailable"
							: pretty(execution.Input)
					}
				/>
			</Container>
			<Container header={<Header variant="h2">Output</Header>}>
				<CodeBlock
					text={
						execution.Output === undefined
							? execution.Status === "RUNNING"
								? "Output will be available when the execution succeeds."
								: "No output returned."
							: pretty(execution.Output)
					}
				/>
			</Container>
		</SpaceBetween>
	);
}
export function ExecutionDetails({
	scope,
	onChanged,
}: {
	scope: SfnScope;
	onChanged: (message: string) => void;
}) {
	const poll = usePoll(() =>
		executionCall<Execution>(scope, "DescribeExecution"),
	);
	const [stop, setStop] = useState(false),
		[error, setError] = useState(""),
		[cause, setCause] = useState("");
	const [reverse, setReverse] = useState(false),
		[includeData, setIncludeData] = useState(true);
	const execution = poll.data;
	return (
		<SpaceBetween size="l">
			<Header
				variant="h1"
				description="Inspect this Standard execution and its recorded state transitions."
				actions={
					<SpaceBetween direction="horizontal" size="xs">
						<Button
							iconName="refresh"
							ariaLabel="Refresh execution"
							onClick={poll.reload}
						/>
						<Button
							disabled={execution?.Status !== "RUNNING" || !!poll.error}
							onClick={() => setStop(true)}
						>
							Stop execution
						</Button>
					</SpaceBetween>
				}
			>
				{scope.execution}
			</Header>
			{poll.error && (
				<Alert type="error" header="Cannot load execution">
					{poll.error}
				</Alert>
			)}
			{poll.loading && <Box>Loading execution…</Box>}
			{execution && (
				<Tabs
					tabs={[
						{
							id: "details",
							label: "Details",
							content: <ExecutionResult execution={execution} />,
						},
						{
							id: "history",
							label: "Execution history",
							content: (
								<SpaceBetween size="l">
									<SpaceBetween direction="horizontal" size="l">
										<Checkbox
											checked={reverse}
											onChange={({ detail }) => setReverse(detail.checked)}
										>
											Newest events first
										</Checkbox>
										<Checkbox
											checked={includeData}
											onChange={({ detail }) => setIncludeData(detail.checked)}
										>
											Include execution data
										</Checkbox>
									</SpaceBetween>
									<ExecutionHistory
										key={`${reverse}/${includeData}`}
										scope={scope}
										reverse={reverse}
										includeData={includeData}
										running={execution.Status === "RUNNING"}
									/>
								</SpaceBetween>
							),
						},
					]}
				/>
			)}
			{stop && (
				<ActionDialog
					title="Stop execution"
					submit="Stop execution"
					confirmation={scope.execution}
					onClose={() => setStop(false)}
					onSaved={() => {
						setStop(false);
						poll.reload();
						onChanged(`Execution ${scope.execution} stopped.`);
					}}
					plan={async () => [
						{
							label: "Stop execution",
							run: () =>
								executionCall(scope, "StopExecution", {
									...(error ? { Error: error } : {}),
									...(cause ? { Cause: cause } : {}),
								}),
						},
					]}
				>
					<SpaceBetween size="l">
						<Alert type="warning">
							Stops this workflow. Actions already performed by tasks are not
							rolled back.
						</Alert>
						<FormField label="Error (optional)">
							<Input
								ariaLabel="Stop error"
								value={error}
								onChange={({ detail }) => setError(detail.value)}
							/>
						</FormField>
						<FormField label="Cause (optional)">
							<Textarea
								ariaLabel="Stop cause"
								value={cause}
								onChange={({ detail }) => setCause(detail.value)}
							/>
						</FormField>
					</SpaceBetween>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
function ExecutionHistory({
	scope,
	reverse,
	includeData,
	running,
}: {
	scope: SfnScope;
	reverse: boolean;
	includeData: boolean;
	running: boolean;
}) {
	const page = useSfnPage<HistoryEvent>(async (NextToken) => {
		const out = await executionCall<{
			Events?: HistoryEvent[];
			NextToken?: string;
		}>(scope, "GetExecutionHistory", {
			MaxResults: 20,
			ReverseOrder: reverse,
			IncludeExecutionData: includeData,
			...(NextToken ? { NextToken } : {}),
		});
		return { rows: out.Events ?? [], next: out.NextToken };
	}, running);
	const [selected, setSelected] = useState<HistoryEvent>();
	return (
		<SpaceBetween size="l">
			{page.error && (
				<Alert type="error" header="Cannot load execution history">
					{page.error}
				</Alert>
			)}
			<Table
				items={page.rows}
				loading={page.loading}
				loadingText="Loading execution history"
				trackBy="Id"
				empty={
					<Box padding="l" textAlign="center">
						No execution events recorded yet.
					</Box>
				}
				header={
					<Header
						variant="h2"
						description="The first page refreshes while the execution is running."
						actions={
							<Button
								iconName="refresh"
								ariaLabel="Refresh history"
								onClick={page.reload}
							/>
						}
					>
						Execution history
					</Header>
				}
				columnDefinitions={[
					{ id: "id", header: "Event ID", cell: (e) => e.Id },
					{
						id: "type",
						header: "Event type",
						cell: (e) => (
							<Button variant="inline-link" onClick={() => setSelected(e)}>
								{e.Type}
							</Button>
						),
					},
					{ id: "state", header: "State name", cell: eventState },
					{ id: "time", header: "Time", cell: (e) => timestamp(e.Timestamp) },
					{
						id: "previous",
						header: "Previous event ID",
						cell: (e) => e.PreviousEventId || "—",
					},
				]}
			/>
			<SpaceBetween direction="horizontal" size="xs">
				<Button
					disabled={!page.hasPrevious || page.loading}
					onClick={page.previous}
				>
					Previous events
				</Button>
				<Box>Page {page.page}</Box>
				<Button disabled={!page.hasNext || page.loading} onClick={page.next}>
					Next events
				</Button>
			</SpaceBetween>
			{selected && (
				<Modal
					visible
					size="large"
					header={`Event ${selected.Id}: ${selected.Type}`}
					closeAriaLabel="Close event details"
					onDismiss={() => setSelected(undefined)}
					footer={
						<Box float="right">
							<Button onClick={() => setSelected(undefined)}>Close</Button>
						</Box>
					}
				>
					<CodeBlock text={JSON.stringify(eventDetails(selected), null, 2)} />
				</Modal>
			)}
		</SpaceBetween>
	);
}
