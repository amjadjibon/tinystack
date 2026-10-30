import {
	Alert,
	Button,
	Container,
	Flashbar,
	FormField,
	Header,
	Input,
	SpaceBetween,
	StatusIndicator,
	Table,
} from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { ConsoleLayout } from "#/components/Console";
import { EmptyState } from "#/components/EmptyState";
import { type ActionStep, errorText } from "#/lib/consoleActions";
import { developer } from "#/lib/developer";
import {
	emptyFaultForm,
	type FaultForm,
	type FaultRule,
	faultFields,
	faultRule,
	initPending,
	latencyValue,
} from "#/lib/developerTools";
import { useScope } from "#/lib/scope";
import { usePoll } from "#/lib/usePoll";

export const Route = createFileRoute("/developer")({ component: Developer });
const labels: Record<keyof FaultForm, string> = {
	service: "Service",
	operation: "Operation",
	region: "Region",
	account: "Account",
	code: "Error code",
	message: "Error message",
	statusCode: "HTTP status (0 or blank = 503)",
	probability: "Probability (blank = 1)",
	latencyMs: "Latency (ms)",
};
function Developer() {
	const scope = useScope();
	const faults = usePoll(developer.faults, 0);
	const effects = usePoll(developer.effects, 0);
	const [pending, setPending] = useState(false);
	const init = usePoll(developer.init, pending ? 2000 : 0);
	// The polling interval follows the latest server state, including scripts discovered after reset.
	const shouldPoll = initPending(init.data);
	useEffect(() => setPending(shouldPoll), [shouldPoll]);
	const [form, setForm] = useState(emptyFaultForm);
	const [latency, setLatency] = useState("");
	const [resetError, setResetError] = useState("");
	const resetDenied =
		scope.session?.sso?.admin === false ||
		resetError.includes("administrator access required");
	const [notice, setNotice] = useState("");
	const [action, setAction] = useState<{
		title: string;
		text: string;
		plan: () => Promise<ActionStep[]>;
		reset?: boolean;
	}>();
	const open = (
		title: string,
		text: string,
		run: () => Promise<unknown>,
		reset = false,
	) =>
		setAction({
			title,
			text,
			plan: async () => [{ label: title, run }],
			reset,
		});
	const remove = (rule: FaultRule) =>
		open(
			"Remove fault rule",
			`Remove this exact rule (all identical copies): ${JSON.stringify(rule)}`,
			() => developer.changeFaults("DELETE", [rule]),
		);
	return (
		<ConsoleLayout
			breadcrumbs={[{ text: "Developer tools", href: "/developer" }]}
			contentType="default"
			content={
				<SpaceBetween size="l">
					<Header variant="h1">Developer tools</Header>
					{notice && (
						<Flashbar
							items={[
								{
									type: "success",
									content: notice,
									dismissible: true,
									onDismiss: () => setNotice(""),
								},
							]}
						/>
					)}
					<Container header={<Header variant="h2">Reset</Header>}>
						<SpaceBetween size="m">
							{(resetDenied || resetError) && (
								<Alert type="error">
									{resetError || "administrator access required"}
								</Alert>
							)}
							<p>
								Clear every account and Region, and restore configured fault
								rules.
							</p>
							<SpaceBetween direction="horizontal" size="xs">
								{[false, true].map((rerun) => (
									<Button
										key={String(rerun)}
										disabled={resetDenied}
										onClick={() =>
											open(
												rerun
													? "Reset and re-run ready scripts"
													: "Reset all state",
												"Every account and Region is cleared, including stored resources, request history, delivery records and mailbox messages. Fault rules return to the configured ones and global latency is cleared. This cannot be undone.",
												async () => {
													try {
														await developer.reset(rerun);
														setResetError("");
													} catch (error) {
														setResetError(errorText(error));
														throw error;
													}
												},
												true,
											)
										}
									>
										{rerun
											? "Reset and re-run ready scripts"
											: "Reset all state"}
									</Button>
								))}
							</SpaceBetween>
						</SpaceBetween>
					</Container>
					<Container
						header={
							<Header
								variant="h2"
								actions={
									<Button
										onClick={() => {
											faults.reload();
											effects.reload();
										}}
									>
										Refresh
									</Button>
								}
							>
								Fault injection
							</Header>
						}
					>
						<SpaceBetween size="l">
							{faults.error && <Alert type="error">{faults.error}</Alert>}
							{!faults.error?.includes("administrator access required") && (
								<>
									<Table
										loading={faults.loading}
										loadingText="Loading fault rules"
										items={faults.data ?? []}
										empty={
											<EmptyState
												title="No fault rules"
												subtitle="Add a rule to inject an error or latency."
											/>
										}
										columnDefinitions={[
											...(
												[
													"service",
													"operation",
													"region",
													"account",
													"probability",
													"latencyMs",
												] as const
											).map((field) => ({
												id: field,
												header: field,
												cell: (r: FaultRule) =>
													String(
														r[field] ??
															(field === "probability"
																? 1
																: field === "latencyMs"
																	? 0
																	: "Any"),
													),
											})),
											{
												id: "code",
												header: "Error code",
												cell: (r) => r.error?.code ?? "—",
											},
											{
												id: "message",
												header: "Error message",
												cell: (r) => r.error?.message ?? "—",
											},
											{
												id: "status",
												header: "HTTP status",
												cell: (r) =>
													r.error ? r.error.statusCode || 503 : "—",
											},
											{
												id: "remove",
												header: "Actions",
												cell: (r) => (
													<Button onClick={() => remove(r)}>Remove</Button>
												),
											},
										]}
									/>
									<Button
										onClick={() =>
											open(
												"Clear all fault rules",
												"Remove every active fault rule. Global latency remains active.",
												() => developer.changeFaults("DELETE", []),
											)
										}
									>
										Clear all
									</Button>
									<SpaceBetween size="m">
										{faultFields.map((field) => (
											<FormField key={field} label={labels[field]}>
												<Input
													value={form[field]}
													onChange={({ detail }) =>
														setForm({ ...form, [field]: detail.value })
													}
												/>
											</FormField>
										))}
										<Button
											onClick={() =>
												setAction({
													title: "Add rule",
													text: "Append this rule to the active rules. Blank selectors match every request.",
													plan: async () => {
														const rule = faultRule(form, faults.data?.length);
														return [
															{
																label: "Add rule",
																run: () =>
																	developer.changeFaults("PATCH", [rule]),
															},
														];
													},
												})
											}
										>
											Add rule
										</Button>
									</SpaceBetween>
								</>
							)}
							{effects.error && <Alert type="error">{effects.error}</Alert>}
							{!effects.error?.includes("administrator access required") && (
								<SpaceBetween size="s">
									<FormField
										label="Global latency (ms)"
										description={
											effects.loading
												? "Loading global latency"
												: `Current: ${effects.data?.latency ?? "unknown"} ms. Zero disables it; maximum 3600000.`
										}
									>
										<Input
											value={latency}
											placeholder={String(effects.data?.latency ?? 0)}
											onChange={({ detail }) => setLatency(detail.value)}
										/>
									</FormField>
									<Button
										onClick={() =>
											setAction({
												title: "Set global latency",
												text: "Apply this latency to all eligible AWS requests.",
												plan: async () => {
													const ms = latencyValue(latency);
													return [
														{
															label: "Set latency",
															run: () => developer.setLatency(ms),
														},
													];
												},
											})
										}
									>
										Set global latency
									</Button>
								</SpaceBetween>
							)}
						</SpaceBetween>
					</Container>
					<Container
						header={
							<Header
								variant="h2"
								actions={<Button onClick={init.reload}>Refresh</Button>}
							>
								Init scripts
							</Header>
						}
					>
						<SpaceBetween size="m">
							{init.error && <Alert type="error">{init.error}</Alert>}
							{!init.error?.includes("administrator access required") && (
								<>
									{Object.entries(init.data?.stages ?? {}).map(
										([stage, state]) => (
											<div key={stage}>
												{stage}:{" "}
												<StatusIndicator
													type={
														state === "ERROR"
															? "error"
															: state === "SUCCESSFUL"
																? "success"
																: state === "RUNNING"
																	? "in-progress"
																	: "pending"
													}
												>
													{state}
												</StatusIndicator>{" "}
												·{" "}
												{init.data?.completed[stage]
													? "Completed"
													: "Not completed"}
											</div>
										),
									)}
									<Table
										loading={init.loading}
										loadingText="Loading init scripts"
										items={init.data?.scripts ?? []}
										empty={
											<EmptyState
												title="No init scripts"
												subtitle="Configure an init directory to run startup scripts."
											/>
										}
										columnDefinitions={[
											...(
												[
													"stage",
													"name",
													"path",
													"state",
													"started_at",
													"finished_at",
													"exit_code",
													"error",
												] as const
											).map((field) => ({
												id: field,
												header: field,
												cell: (
													s: NonNullable<typeof init.data>["scripts"][number],
												) => String(s[field] ?? "—"),
											})),
											{
												id: "output",
												header: "Output excerpt",
												cell: (s) =>
													s.output ? (
														<CodeBlock text={s.output.slice(0, 4096)} />
													) : (
														"—"
													),
											},
										]}
									/>
								</>
							)}
						</SpaceBetween>
					</Container>
					{action && (
						<ActionDialog
							title={action.title}
							submit={action.title}
							plan={action.plan}
							onClose={() => setAction(undefined)}
							onSaved={() => {
								setNotice(`${action.title} succeeded.`);
								if (action.reset) scope.inventory.reload();
								setAction(undefined);
								faults.reload();
								effects.reload();
								init.reload();
							}}
						>
							<p>{action.text}</p>
						</ActionDialog>
					)}
				</SpaceBetween>
			}
		/>
	);
}
