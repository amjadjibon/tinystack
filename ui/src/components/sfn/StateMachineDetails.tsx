import {
	Alert,
	Box,
	Button,
	Container,
	FormField,
	Header,
	Input,
	KeyValuePairs,
	Link,
	Select,
	SpaceBetween,
	Table,
	Tabs,
	Textarea,
} from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { ConsoleTags } from "#/components/ConsoleTags";
import { pretty, type Resource } from "#/lib/api";
import { iamHref } from "#/lib/iam";
import { logsHref } from "#/lib/logs";
import {
	type Execution,
	executionInput,
	type Machine,
	machineArn,
	machineCall,
	type SfnScope,
	sfnCall,
	sfnHref,
	tagPlan,
	timestamp,
	useSfnPage,
} from "#/lib/sfn";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { ExecutionResult, ExecutionStatus } from "./ExecutionDetails";
import { StateMachineForm } from "./StateMachineForm";

export function StateMachineDetails({
	scope,
	resource,
	resources,
	enabled,
	onChanged,
	onDeleted,
	onExecution,
}: {
	scope: SfnScope;
	resource?: Resource;
	resources: Resource[];
	enabled: boolean;
	onChanged: (message: string) => void;
	onDeleted: () => void;
	onExecution: (name: string) => void;
}) {
	const follow = useFollow();
	const poll = usePoll(async () =>
		enabled
			? {
					machine: await machineCall<Machine>(scope, "DescribeStateMachine"),
					tags: await sfnCall<{ Tags?: { Key: string; Value: string }[] }>(
						scope,
						"ListTagsForResource",
						{ ResourceArn: machineArn(scope) },
					),
				}
			: undefined,
	);
	const [dialog, setDialog] = useState("");
	const [tab, setTab] = useState("executions");
	const [expressResult, setExpressResult] = useState<Execution>();
	const machine = poll.data?.machine;
	const kind = machine?.Type ?? String(resource?.details?.Type ?? "STANDARD");
	const tags = poll.data
		? Object.fromEntries(
				(poll.data.tags.Tags ?? []).map((t) => [t.Key, t.Value]),
			)
		: (resource?.tags ?? {});
	const saved = (message: string) => {
		setDialog("");
		poll.reload();
		onChanged(message);
	};
	const logArn =
		machine?.LoggingConfiguration?.Destinations?.[0]?.CloudWatchLogsLogGroup
			?.LogGroupArn;
	const logMatch = logArn?.match(
		/^arn:[^:]+:logs:([^:]+):(\d{12}):log-group:(.+):\*$/,
	);
	const roleArn = machine?.RoleArn ?? String(resource?.details?.RoleArn ?? "");
	const roleMatch = roleArn.match(/^arn:[^:]+:iam::(\d{12}):role\/(.+)$/);
	return (
		<SpaceBetween size="l">
			<Header
				variant="h1"
				description={`${kind === "EXPRESS" ? "Express" : "Standard"} workflow`}
				actions={
					<SpaceBetween direction="horizontal" size="xs">
						<Button
							iconName="refresh"
							ariaLabel="Refresh state machine"
							onClick={poll.reload}
						/>
						<Button
							disabled={!enabled || !machine}
							onClick={() => setDialog("edit")}
						>
							Edit
						</Button>
						<Button
							disabled={!enabled || !machine}
							onClick={() => setDialog("delete")}
						>
							Delete
						</Button>
						<Button
							variant="primary"
							disabled={!enabled || !machine || !!poll.error}
							onClick={() => setDialog("start")}
						>
							{kind === "EXPRESS"
								? "Start synchronous execution"
								: "Start execution"}
						</Button>
					</SpaceBetween>
				}
			>
				{scope.machine}
			</Header>
			{poll.error && (
				<Alert type="error" header="Cannot load state machine">
					{poll.error}
				</Alert>
			)}
			{!enabled && (
				<Alert type="info">
					Only state machine inventory is available in this server mode. Use
					signed AWS CLI or SDK requests to read definitions and executions.
				</Alert>
			)}
			{enabled && poll.loading && <Box>Loading state machine…</Box>}
			<Container header={<Header variant="h2">State machine details</Header>}>
				<KeyValuePairs
					columns={3}
					items={[
						{
							label: "ARN",
							value:
								machine?.StateMachineArn ?? resource?.arn ?? machineArn(scope),
						},
						{ label: "Type", value: kind },
						{ label: "Status", value: machine?.Status ?? "—" },
						{ label: "Created", value: timestamp(machine?.CreationDate) },
						{
							label: "Execution role",
							value: roleMatch ? (
								<Link
									href={iamHref(
										"role",
										roleMatch[1],
										roleMatch[2].split("/").at(-1) ?? "",
									)}
									onFollow={follow}
								>
									{roleArn}
								</Link>
							) : (
								roleArn || "—"
							),
						},
						{ label: "Revision", value: machine?.RevisionId ?? "—" },
					]}
				/>
			</Container>
			<Tabs
				activeTabId={tab}
				onChange={({ detail }) => setTab(detail.activeTabId)}
				tabs={[
					{
						id: "executions",
						label: "Executions",
						content:
							enabled && machine ? (
								kind === "EXPRESS" ? (
									<SpaceBetween size="l">
										<Alert type="info">
											Express execution history is not retained in TinyStack.
											Start a synchronous execution to inspect its result here.
											The result is available until you leave or reload this
											page.
										</Alert>
										{expressResult && (
											<ExecutionResult execution={expressResult} />
										)}
									</SpaceBetween>
								) : (
									<ExecutionList scope={scope} />
								)
							) : (
								<Box>Execution reads are unavailable.</Box>
							),
					},
					{
						id: "definition",
						label: "Definition",
						content: (
							<Container
								header={
									<Header
										variant="h2"
										actions={
											<Button
												disabled={!enabled || !machine}
												onClick={() => setDialog("edit")}
											>
												Edit definition
											</Button>
										}
									>
										Amazon States Language
									</Header>
								}
							>
								<CodeBlock
									text={
										machine
											? pretty(machine.Definition)
											: "Definition reads are unavailable."
									}
								/>
							</Container>
						),
					},
					{
						id: "configuration",
						label: "Configuration",
						content: (
							<Container
								header={<Header variant="h2">Logging and tracing</Header>}
							>
								<KeyValuePairs
									items={[
										{
											label: "Log level",
											value: machine?.LoggingConfiguration?.Level ?? "—",
										},
										{
											label: "Include execution data",
											value: machine
												? String(
														machine.LoggingConfiguration
															?.IncludeExecutionData ?? false,
													)
												: "—",
										},
										{
											label: "Log group",
											value: logMatch ? (
												<Link
													href={logsHref({
														region: logMatch[1],
														account: logMatch[2],
														group: logMatch[3],
													})}
													onFollow={follow}
												>
													{logArn}
												</Link>
											) : (
												(logArn ?? "—")
											),
										},
										{
											label: "Tracing configuration",
											value: machine
												? machine.TracingConfiguration?.Enabled
													? "Enabled (stored configuration)"
													: "Disabled"
												: "—",
										},
									]}
								/>
							</Container>
						),
					},
					{
						id: "tags",
						label: "Tags",
						content: (
							<ConsoleTags
								tags={tags}
								enabled={enabled && !!machine && !poll.error}
								plan={(next, removed) => tagPlan(scope, next, removed)}
								onChanged={() => saved("State machine tags updated.")}
							/>
						),
					},
				]}
			/>
			{dialog === "edit" && machine && (
				<StateMachineForm
					scope={scope}
					machine={machine}
					resources={resources}
					onClose={() => setDialog("")}
					onSaved={() => saved("State machine updated.")}
				/>
			)}
			{dialog === "delete" && (
				<ActionDialog
					title="Delete state machine"
					submit="Delete"
					confirmation={scope.machine}
					onClose={() => setDialog("")}
					onSaved={onDeleted}
					plan={async () => [
						{
							label: "Delete state machine",
							run: () => machineCall(scope, "DeleteStateMachine"),
						},
					]}
				>
					<Alert type="warning">
						TinyStack immediately deletes this state machine, its executions and
						history, versions, and aliases. Running executions are stopped.
					</Alert>
				</ActionDialog>
			)}
			{dialog === "start" && (
				<StartExecution
					scope={scope}
					express={kind === "EXPRESS"}
					onClose={() => setDialog("")}
					onSaved={(result) => {
						setDialog("");
						if (kind === "EXPRESS") {
							setExpressResult(result);
							setTab("executions");
						} else onExecution(result.ExecutionArn.split(":").at(-1) ?? "");
					}}
				/>
			)}
		</SpaceBetween>
	);
}
function StartExecution({
	scope,
	express,
	onClose,
	onSaved,
}: {
	scope: SfnScope;
	express: boolean;
	onClose: () => void;
	onSaved: (result: Execution) => void;
}) {
	const [name, setName] = useState<string>(() => crypto.randomUUID()),
		[input, setInput] = useState("{}");
	const result = useRef<Execution | undefined>(undefined);
	return (
		<ActionDialog
			title={express ? "Start synchronous execution" : "Start execution"}
			submit={express ? "Start synchronous execution" : "Start execution"}
			onClose={onClose}
			onSaved={() => {
				if (result.current) onSaved(result.current);
			}}
			plan={async () => {
				if (!name || Array.from(name).length > 80)
					throw new Error("Execution name must contain 1–80 characters.");
				const Input = executionInput(input);
				return [
					{
						label: "Start execution",
						run: async () => {
							result.current = await machineCall<Execution>(
								scope,
								express ? "StartSyncExecution" : "StartExecution",
								{ Name: name, Input },
							);
						},
					},
				];
			}}
		>
			<SpaceBetween size="l">
				<Alert type={express ? "warning" : "info"}>
					{express
						? "Waits for an Express result. Retrying after a connection failure may run tasks again. Leaving the page does not stop the execution; Express executions cannot be stopped from this console."
						: "Starts this workflow and performs its configured tasks. Use a unique name for each execution."}
				</Alert>
				<FormField label="Execution name" constraintText="1–80 characters.">
					<Input
						ariaLabel="Execution name"
						value={name}
						onChange={({ detail }) => setName(detail.value)}
					/>
				</FormField>
				<FormField
					label="Input (JSON)"
					constraintText="Any valid JSON value, at most 256 KiB."
				>
					<Textarea
						ariaLabel="Execution input"
						rows={10}
						value={input}
						onChange={({ detail }) => setInput(detail.value)}
					/>
				</FormField>
			</SpaceBetween>
		</ActionDialog>
	);
}
function ExecutionList({ scope }: { scope: SfnScope }) {
	const [status, setStatus] = useState("");
	return (
		<SpaceBetween size="l">
			<FormField label="Execution status">
				<Select
					ariaLabel="Execution status filter"
					selectedOption={{ label: status || "All statuses", value: status }}
					options={[
						"",
						"RUNNING",
						"SUCCEEDED",
						"FAILED",
						"TIMED_OUT",
						"ABORTED",
					].map((value) => ({ label: value || "All statuses", value }))}
					onChange={({ detail }) =>
						setStatus(detail.selectedOption.value ?? "")
					}
				/>
			</FormField>
			<ExecutionPage key={status} scope={scope} status={status} />
		</SpaceBetween>
	);
}
function ExecutionPage({ scope, status }: { scope: SfnScope; status: string }) {
	const follow = useFollow();
	const page = useSfnPage<Execution>(async (NextToken) => {
		const out = await machineCall<{
			Executions?: Execution[];
			NextToken?: string;
		}>(scope, "ListExecutions", {
			MaxResults: 20,
			...(status ? { StatusFilter: status } : {}),
			...(NextToken ? { NextToken } : {}),
		});
		return { rows: out.Executions ?? [], next: out.NextToken };
	}, true);
	return (
		<SpaceBetween size="l">
			{page.error && (
				<Alert type="error" header="Cannot load executions">
					{page.error}
				</Alert>
			)}
			<Table
				items={page.rows}
				loading={page.loading}
				loadingText="Loading executions"
				trackBy="ExecutionArn"
				header={
					<Header
						variant="h2"
						description="Newest executions first. The first page refreshes every five seconds."
						actions={
							<Button
								iconName="refresh"
								ariaLabel="Refresh executions"
								onClick={page.reload}
							/>
						}
					>
						Executions
					</Header>
				}
				empty={
					<Box padding="l" textAlign="center">
						No executions. Start an execution to run this workflow.
					</Box>
				}
				columnDefinitions={[
					{
						id: "name",
						header: "Name",
						cell: (e) => (
							<Link
								href={sfnHref({ ...scope, execution: e.Name })}
								onFollow={follow}
							>
								{e.Name}
							</Link>
						),
					},
					{
						id: "status",
						header: "Status",
						cell: (e) => <ExecutionStatus status={e.Status} />,
					},
					{
						id: "started",
						header: "Started",
						cell: (e) => timestamp(e.StartDate),
					},
					{
						id: "finished",
						header: "Finished",
						cell: (e) => timestamp(e.StopDate),
					},
				]}
			/>
			<SpaceBetween direction="horizontal" size="xs">
				<Button
					disabled={!page.hasPrevious || page.loading}
					onClick={page.previous}
				>
					Previous executions
				</Button>
				<Box>Page {page.page}</Box>
				<Button disabled={!page.hasNext || page.loading} onClick={page.next}>
					Next executions
				</Button>
			</SpaceBetween>
		</SpaceBetween>
	);
}
