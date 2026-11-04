import {
	Alert,
	Container,
	Header,
	Link,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { Action, Buttons, Field, Rows } from "#/components/containers/Controls";
import { Copy } from "#/components/efs/Shared";
import { Details } from "#/components/elbv2/Shared";
import { batchCall, childJobs, describeJob } from "#/lib/batch";
import {
	arnName,
	type BatchScope,
	batchHref,
	cancellable,
	countText,
	type Job,
	jobLogGroup,
	jobPending,
	resources,
	terminable,
} from "#/lib/batchHelpers";
import { unixMillis } from "#/lib/format";
import { logsHref } from "#/lib/logs";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { JobRows } from "./JobRows";
import { DetailTabs, ResourceTags, Status } from "./Shared";

export function JobDetails({
	scope,
	enabled,
	docker,
	id,
	queue,
	status,
	tab,
	onBack,
	onChanged,
}: {
	scope: BatchScope;
	enabled: boolean;
	docker: boolean;
	id: string;
	// The queue and status of the list the job was opened from.
	queue: string;
	status: string;
	tab: string;
	onBack: () => void;
	onChanged: () => void;
}) {
	const [changing, setChanging] = useState(true),
		[dialog, setDialog] = useState<"" | "cancel" | "terminate">("");
	const poll = usePoll(
		async () => {
			if (!enabled) return undefined;
			const job = await describeJob(scope, id);
			setChanging(jobPending(job.Status, docker));
			return job;
		},
		changing ? 2000 : 0,
	);
	const job = poll.data;
	const saved = () => {
		setDialog("");
		setChanging(true);
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<Buttons>
				<Action onClick={onBack}>Jobs</Action>
				<Action onClick={poll.reload}>Refresh</Action>
				<Action
					disabled={!enabled || !cancellable(job?.Status)}
					onClick={() => setDialog("cancel")}
				>
					Cancel job
				</Action>
				<Action
					disabled={!enabled || !terminable(job?.Status)}
					onClick={() => setDialog("terminate")}
				>
					Terminate job
				</Action>
			</Buttons>
			{!!job && (
				<DetailTabs
					tab={tab}
					href={(next) =>
						batchHref(scope, "jobs", { queue, status, job: id, tab: next })
					}
					overview={
						<Overview
							scope={scope}
							docker={docker}
							job={job}
							queue={queue}
							status={status}
						/>
					}
					tags={
						<ResourceTags
							scope={scope}
							arn={job.JobArn ?? ""}
							tags={job.Tags}
							enabled={enabled && !!job.JobArn}
							onChanged={saved}
						/>
					}
				/>
			)}
			{dialog !== "" && !!job && (
				<EndDialog
					scope={scope}
					job={job}
					terminate={dialog === "terminate"}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
		</SpaceBetween>
	);
}

// EndDialog cancels a job that has not started, or terminates one that has.
function EndDialog({
	scope,
	job,
	terminate,
	onClose,
	onSaved,
}: {
	scope: BatchScope;
	job: Job;
	terminate: boolean;
	onClose: () => void;
	onSaved: () => void;
}) {
	const operation = terminate ? "TerminateJob" : "CancelJob";
	const [reason, setReason] = useState(
		terminate ? "Terminated from the console" : "Cancelled from the console",
	);
	return (
		<ActionDialog
			title={terminate ? "Terminate job" : "Cancel job"}
			submit={terminate ? "Terminate job" : "Cancel job"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				if (!reason.trim()) throw new Error("Enter a reason.");
				return [
					{
						label: operation,
						run: () =>
							batchCall(scope, operation, {
								JobId: job.JobId,
								Reason: reason.trim(),
							}),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Alert type="warning">
					{terminate
						? `Terminate ${job.JobName ?? job.JobId} and fail it, including its running container. An array job also ends its children.`
						: `Cancel ${job.JobName ?? job.JobId} and fail it. A PENDING job is cancelled once the jobs it depends on finish. Cancel does not stop a job that is STARTING or RUNNING; terminate it instead.`}
				</Alert>
				<Field label="Reason" value={reason} onChange={setReason} />
			</SpaceBetween>
		</ActionDialog>
	);
}

function Overview({
	scope,
	docker,
	job,
	queue,
	status,
}: {
	scope: BatchScope;
	docker: boolean;
	job: Job;
	queue: string;
	status: string;
}) {
	const follow = useFollow();
	const c = job.Container;
	const need = resources(c);
	const array = job.ArrayProperties;
	const parent = array && array.Index === undefined;
	const link = (href: string, text: string) => (
		<Link key={href} href={href} onFollow={follow}>
			{text}
		</Link>
	);
	const logs = (stream?: string) =>
		stream ? (
			<Link
				key={stream}
				href={logsHref({
					account: scope.account,
					region: scope.region,
					group: jobLogGroup(job),
					stream,
				})}
				onFollow={follow}
			>
				{stream}
			</Link>
		) : (
			"-"
		);
	return (
		<SpaceBetween size="l">
			<Container header={<Header variant="h2">Job</Header>}>
				<Details
					items={[
						["Job ID", <Copy key="id" text={job.JobId} label="ID" />],
						["ARN", <Copy key="arn" text={job.JobArn} label="ARN" />],
						["Name", job.JobName],
						["Status", <Status key="status" value={job.Status} />],
						["Status reason", job.StatusReason],
						[
							"Job queue",
							job.JobQueue
								? link(
										batchHref(scope, "queues", {
											queue: arnName(job.JobQueue),
										}),
										arnName(job.JobQueue),
									)
								: "-",
						],
						[
							"Job definition",
							job.JobDefinition
								? link(
										batchHref(scope, "definitions", {
											definition: arnName(job.JobDefinition),
										}),
										arnName(job.JobDefinition),
									)
								: "-",
						],
						["Created", unixMillis(job.CreatedAt)],
						["Started", unixMillis(job.StartedAt)],
						["Stopped", unixMillis(job.StoppedAt)],
						["Platform", job.PlatformCapabilities?.join(", ")],
						["Retry attempts", countText(job.RetryStrategy?.Attempts)],
						[
							"Timeout (seconds)",
							countText(job.Timeout?.AttemptDurationSeconds),
						],
						[
							"Array",
							array
								? `${array.Index === undefined ? "Parent" : `Child ${array.Index}`} of ${countText(array.Size)}`
								: "-",
						],
					]}
				/>
			</Container>
			<Container header={<Header variant="h2">Container</Header>}>
				<Details
					items={[
						["Image", c?.Image],
						["Command", c?.Command ? JSON.stringify(c.Command) : "-"],
						["vCPUs", countText(need.vcpus)],
						["Memory (MiB)", countText(need.memory)],
						["Exit code", countText(c?.ExitCode)],
						["Reason", c?.Reason],
						["Job role", c?.JobRoleArn],
						["Execution role", c?.ExecutionRoleArn],
						["Assign public IP", c?.NetworkConfiguration?.AssignPublicIp],
						[
							"Fargate platform version",
							c?.FargatePlatformConfiguration?.PlatformVersion,
						],
						["CPU architecture", c?.RuntimePlatform?.CpuArchitecture],
						["Operating system", c?.RuntimePlatform?.OperatingSystemFamily],
						["Task ARN", c?.TaskArn],
						["Log group", jobLogGroup(job)],
						["Log stream", logs(c?.LogStreamName)],
					]}
				/>
				{!c?.LogStreamName && (
					<Alert>
						{docker
							? "The log stream appears once the job starts."
							: "Records mode starts no container, so the job has no log stream."}
					</Alert>
				)}
			</Container>
			{!!job.Attempts?.length && (
				<Rows
					title="Attempts"
					items={job.Attempts.map((a, i) => ({ ...a, number: i + 1 }))}
					emptyText="The job has not started."
					columns={[
						{ id: "number", header: "Attempt", cell: (a) => a.number },
						{
							id: "started",
							header: "Started",
							cell: (a) => unixMillis(a.StartedAt),
						},
						{
							id: "stopped",
							header: "Stopped",
							cell: (a) => unixMillis(a.StoppedAt),
						},
						{
							id: "exit",
							header: "Exit code",
							cell: (a) => countText(a.Container?.ExitCode),
						},
						{
							id: "reason",
							header: "Reason",
							cell: (a) => a.StatusReason ?? a.Container?.Reason ?? "-",
						},
						{
							id: "stream",
							header: "Log stream",
							cell: (a) => logs(a.Container?.LogStreamName),
						},
					]}
				/>
			)}
			{parent && (
				<Children
					scope={scope}
					docker={docker}
					job={job}
					queue={queue}
					status={status}
				/>
			)}
			{!!Object.keys(job.Parameters ?? {}).length && (
				<Container header={<Header variant="h3">Parameters</Header>}>
					<CodeBlock text={JSON.stringify(job.Parameters, null, 2)} />
				</Container>
			)}
			{!!c?.Environment?.length && (
				<Container header={<Header variant="h3">Environment</Header>}>
					<CodeBlock
						text={JSON.stringify(
							Object.fromEntries(
								c.Environment.map((e) => [e.Name ?? "", e.Value ?? ""]),
							),
							null,
							2,
						)}
					/>
				</Container>
			)}
			{!!job.DependsOn?.length && (
				<Container header={<Header variant="h3">Depends on</Header>}>
					<CodeBlock text={JSON.stringify(job.DependsOn, null, 2)} />
				</Container>
			)}
		</SpaceBetween>
	);
}

// Children lists the jobs an array job runs and how many are in each status.
function Children({
	scope,
	docker,
	job,
	queue,
	status,
}: {
	scope: BatchScope;
	docker: boolean;
	job: Job;
	queue: string;
	status: string;
}) {
	const [changing, setChanging] = useState(true);
	const poll = usePoll(
		async () => {
			const rows = await childJobs(scope, job.JobId);
			setChanging(rows.some((j) => jobPending(j.Status, docker)));
			return rows;
		},
		changing ? 2000 : 0,
	);
	const summary = job.ArrayProperties?.StatusSummary ?? {};
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<JobRows
				title="Child jobs"
				items={poll.data ?? []}
				loading={poll.loading}
				emptyText="The array job has no children."
				href={(j) => batchHref(scope, "jobs", { queue, status, job: j.JobId })}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
					</Buttons>
				}
			/>
			{!!Object.keys(summary).length && (
				<Details
					items={Object.entries(summary).map(
						([name, count]): [string, string] => [name, String(count)],
					)}
				/>
			)}
		</SpaceBetween>
	);
}
