import {
	Alert,
	Box,
	Container,
	Header,
	Link,
	SpaceBetween,
} from "@cloudscape-design/components";
import { Rows } from "#/components/containers/Controls";
import {
	type Build,
	type Project,
	pending,
	type Scope,
} from "#/lib/codebuildHelpers";
import { unixTime } from "#/lib/format";
import { type LogEvent, logsCall, logsHref } from "#/lib/logs";
import { tagMap } from "#/lib/tags";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
export function Environment({
	environment,
}: {
	environment?: Project["Environment"];
}) {
	return (
		<Rows
			title="Environment variables"
			items={environment?.EnvironmentVariables ?? []}
			emptyText="Edit the project to add variables."
			columns={[
				{ id: "name", header: "Name", cell: (v) => v.Name },
				{ id: "type", header: "Type", cell: (v) => v.Type },
				{ id: "value", header: "Value / reference", cell: (v) => v.Value },
			]}
		/>
	);
}
export function ProjectDetails({ project: p }: { project: Project }) {
	return (
		<SpaceBetween size="m">
			<Container header={<Header>{p.Name}</Header>}>
				<SpaceBetween size="s">
					<Box>ARN: {p.Arn}</Box>
					<Box>Description: {p.Description || "-"}</Box>
					<Box>Created: {unixTime(p.Created)}</Box>
					<Box>Updated: {unixTime(p.LastModified)}</Box>
					<Box>Role: {p.ServiceRole}</Box>
					<Box>
						Build / queue timeout: {p.TimeoutInMinutes} /{" "}
						{p.QueuedTimeoutInMinutes} minutes
					</Box>
					<Box>Source version: {p.SourceVersion || "-"}</Box>
					<Box>
						Image: {p.Environment.Image}; {p.Environment.Type};{" "}
						{p.Environment.ComputeType}; privileged:{" "}
						{String(p.Environment.PrivilegedMode ?? false)}
					</Box>
					<Box>Source</Box>
					<pre>{JSON.stringify(p.Source, null, 2)}</pre>
					<Box>Artifacts</Box>
					<pre>{JSON.stringify(p.Artifacts, null, 2)}</pre>
					<Box>CloudWatch logs</Box>
					<pre>{JSON.stringify(p.LogsConfig ?? {}, null, 2)}</pre>
					<Box>Tags</Box>
					<pre>{JSON.stringify(tagMap(p.Tags), null, 2)}</pre>
				</SpaceBetween>
			</Container>
			<Environment environment={p.Environment} />
		</SpaceBetween>
	);
}
export function BuildDetails({
	build: b,
	scope,
	logsEnabled,
}: {
	build: Build;
	scope: Scope;
	logsEnabled: boolean;
}) {
	const follow = useFollow();
	const logs = usePoll(
		async () => {
			if (!logsEnabled || !b.Logs?.GroupName || !b.Logs.StreamName) return [];
			const out = await logsCall<{ Events?: LogEvent[] }>(
				scope,
				"GetLogEvents",
				{
					LogGroupName: b.Logs.GroupName,
					LogStreamName: b.Logs.StreamName,
					Limit: 1000,
				},
			);
			return out.Events ?? [];
		},
		pending(b) ? 2000 : 0,
	);
	return (
		<SpaceBetween size="m">
			<Container header={<Header>{b.Id}</Header>}>
				<SpaceBetween size="s">
					<Box>
						Status: {b.BuildStatus}; phase: {b.CurrentPhase ?? "-"}
					</Box>
					<Box>Started: {unixTime(b.StartTime)}</Box>
					<Box>Ended: {unixTime(b.EndTime)}</Box>
					<Box>Source version: {b.SourceVersion || "-"}</Box>
					<Box>ARN: {b.Arn ?? "-"}</Box>
				</SpaceBetween>
			</Container>
			<Rows
				title="Phases"
				items={b.Phases ?? []}
				emptyText="Phases appear when a build starts."
				columns={[
					{ id: "phase", header: "Phase", cell: (p) => p.PhaseType },
					{ id: "status", header: "Status", cell: (p) => p.PhaseStatus ?? "-" },
					{
						id: "start",
						header: "Started",
						cell: (p) => unixTime(p.StartTime),
					},
					{ id: "end", header: "Ended", cell: (p) => unixTime(p.EndTime) },
					{
						id: "duration",
						header: "Duration (seconds)",
						cell: (p) => p.DurationInSeconds ?? "-",
					},
					{
						id: "context",
						header: "Context",
						cell: (p) => p.Contexts?.map((c) => c.Message).join("; ") || "-",
					},
				]}
			/>
			<Environment environment={b.Environment} />
			<Container header={<Header>Logs</Header>}>
				<SpaceBetween size="s">
					{!!b.Logs?.GroupName && (
						<Link
							href={logsHref({
								...scope,
								group: b.Logs.GroupName,
								stream: b.Logs.StreamName,
							})}
							onFollow={follow}
						>
							Open CloudWatch Logs: {b.Logs.GroupName} / {b.Logs.StreamName}
						</Link>
					)}
					{!!logs.error && <Alert type="error">{logs.error}</Alert>}
					{!b.Logs?.GroupName && (
						<Box>CloudWatch logging is disabled for this build.</Box>
					)}
					{!!b.Logs?.GroupName && !logsEnabled && (
						<Box>
							Connect to a server with the Logs page enabled to read output.
						</Box>
					)}
					<Box>Latest log output (up to 1000 events)</Box>
					<pre style={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}>
						{logs.data?.map((e) => e.Message).join("\n") ||
							"No log events available."}
					</pre>
				</SpaceBetween>
			</Container>
		</SpaceBetween>
	);
}
