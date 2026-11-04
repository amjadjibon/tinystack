import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	Container,
	ContentLayout,
	FormField,
	Header,
	Input,
	KeyValuePairs,
	Link,
	Pagination,
	Select,
	SpaceBetween,
	Table,
	Tabs,
	Textarea,
	TextFilter,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { ConsoleTags } from "#/components/ConsoleTags";
import type { Resource } from "#/lib/api";
import { integer } from "#/lib/consoleActions";
import {
	groupCall,
	type LogGroup,
	type LogStream,
	type LogsScope,
	logGroup,
	logStreams,
	logsArn,
	logsCall,
	logsHref,
	logTime,
	retentionDays,
} from "#/lib/logs";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { LogEvents } from "./LogEvents";
import { LogsInsights } from "./LogsInsights";
export function LogGroupDetails({
	scope,
	resource,
	groups,
	enabled,
	onChanged,
	onDeleted,
	onStreamDeleted,
}: {
	scope: LogsScope;
	resource?: Resource;
	groups: Resource[];
	enabled: boolean;
	onChanged: (message: string) => void;
	onDeleted: () => void;
	onStreamDeleted: () => void;
}) {
	const follow = useFollow();
	const overview = usePoll(async () =>
		enabled
			? ((
					await logsCall<{ LogGroups?: LogGroup[] }>(
						scope,
						"DescribeLogGroups",
						{ LogGroupNamePrefix: scope.group, Limit: 1 },
					)
				).LogGroups?.find((g) => g.LogGroupName === scope.group) ?? {})
			: logGroup(resource),
	);
	const streams = usePoll(
		async () =>
			enabled
				? await logStreams(scope)
				: ((resource?.details?.Streams ?? []) as string[]).map(
						(LogStreamName) => ({ LogStreamName }),
					),
		15000,
	);
	const tags = usePoll(async () =>
		enabled
			? ((
					await logsCall<{ Tags?: Record<string, string> }>(
						scope,
						"ListTagsForResource",
						{ ResourceArn: logsArn(scope) },
					)
				).Tags ?? {})
			: (resource?.tags ?? {}),
	);
	const data = overview.data ?? logGroup(resource);
	const [eventsVersion, setEventsVersion] = useState(0);
	const [dialog, setDialog] = useState<
			| "retention"
			| "delete"
			| "create-stream"
			| "delete-stream"
			| "write"
			| null
		>(null),
		[retention, setRetention] = useState("0"),
		[streamName, setStreamName] = useState("");
	const [message, setMessage] = useState(""),
		[timestamp, setTimestamp] = useState("");
	const refresh = (notice: string) => {
		overview.reload();
		streams.reload();
		tags.reload();
		onChanged(notice);
	};
	const rows: LogStream[] = streams.data ?? [];
	const collection = useCollection(rows, {
		filtering: {},
		pagination: { pageSize: 20 },
		sorting: {
			defaultState: { sortingColumn: { sortingField: "LogStreamName" } },
		},
	});
	return (
		<ContentLayout
			header={
				<Header
					variant="h1"
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button
								iconName="refresh"
								ariaLabel="Refresh log group"
								onClick={() => refresh("")}
							/>
							{scope.stream ? (
								<SpaceBetween direction="horizontal" size="xs">
									<Button
										disabled={!enabled}
										onClick={() => {
											setMessage("");
											setTimestamp("");
											setDialog("write");
										}}
									>
										Add log event
									</Button>
									<Button
										disabled={!enabled}
										onClick={() => setDialog("delete-stream")}
									>
										Delete stream
									</Button>
								</SpaceBetween>
							) : (
								<SpaceBetween direction="horizontal" size="xs">
									<Button
										disabled={!enabled || !data.LogGroupName}
										onClick={() => {
											setRetention(String(data.RetentionInDays ?? 0));
											setDialog("retention");
										}}
									>
										Edit retention
									</Button>
									<Button
										disabled={!enabled || !data.LogGroupName}
										onClick={() => setDialog("delete")}
									>
										Delete group
									</Button>
								</SpaceBetween>
							)}
						</SpaceBetween>
					}
				>
					{scope.stream ?? scope.group}
				</Header>
			}
		>
			<SpaceBetween size="l">
				{!!(overview.error || streams.error || tags.error) && (
					<Alert type="error">
						{overview.error || streams.error || tags.error}
					</Alert>
				)}
				{!overview.loading && !data.LogGroupName && (
					<Alert type="warning">
						This log group was not found in the selected account and Region.
					</Alert>
				)}
				{scope.stream ? (
					<LogEvents
						key={`${scope.account}/${scope.region}/${scope.group}/${scope.stream}/${enabled && !!data.LogGroupName}/${eventsVersion}`}
						scope={scope}
						enabled={enabled && !!data.LogGroupName}
					/>
				) : (
					<SpaceBetween size="l">
						<Container header={<Header variant="h2">Log group details</Header>}>
							<KeyValuePairs
								columns={3}
								items={[
									{ label: "ARN", value: data.Arn ?? logsArn(scope) },
									{
										label: "Retention",
										value: data.RetentionInDays
											? `${data.RetentionInDays} days`
											: "Never expire",
									},
									{ label: "Class", value: data.LogGroupClass ?? "STANDARD" },
									{ label: "Stored bytes", value: data.StoredBytes ?? 0 },
									{
										label: "Created",
										value:
											data.CreationTime !== undefined
												? new Date(data.CreationTime).toLocaleString()
												: "—",
									},
								]}
							/>
						</Container>
						<Tabs
							tabs={[
								{
									id: "streams",
									label: "Log streams",
									content: (
										<Table
											{...collection.collectionProps}
											items={collection.items}
											loading={streams.loading}
											wrapLines
											trackBy="LogStreamName"
											header={
												<Header
													variant="h2"
													counter={`(${rows.length})`}
													actions={
														<Button
															variant="primary"
															disabled={!enabled || !data.LogGroupName}
															onClick={() => {
																setStreamName("");
																setDialog("create-stream");
															}}
														>
															Create log stream
														</Button>
													}
												>
													Log streams
												</Header>
											}
											filter={
												<TextFilter
													{...collection.filterProps}
													filteringPlaceholder="Find log streams"
												/>
											}
											pagination={
												<Pagination {...collection.paginationProps} />
											}
											columnDefinitions={[
												{
													id: "name",
													header: "Log stream",
													sortingField: "LogStreamName",
													cell: (s) => (
														<Link
															href={logsHref({
																...scope,
																stream: s.LogStreamName,
															})}
															onFollow={follow}
														>
															{s.LogStreamName}
														</Link>
													),
												},
												{
													id: "latest",
													header: "Last event",
													sortingField: "LastEventTimestamp",
													cell: (s) =>
														s.LastEventTimestamp !== undefined
															? new Date(s.LastEventTimestamp).toLocaleString()
															: "—",
												},
												{
													id: "created",
													header: "Created",
													cell: (s) =>
														s.CreationTime !== undefined
															? new Date(s.CreationTime).toLocaleString()
															: "—",
												},
											]}
											empty={
												<Box textAlign="center" padding="l">
													No matching log streams. Create a stream or run a
													Lambda function that writes to this group.
												</Box>
											}
										/>
									),
								},
								{
									id: "search",
									label: "Search events",
									content: (
										<LogEvents
											scope={scope}
											enabled={enabled && !!data.LogGroupName}
										/>
									),
								},
								{
									id: "insights",
									label: "Logs Insights",
									content: (
										<LogsInsights
											scope={scope}
											groups={groups}
											enabled={enabled && !!data.LogGroupName}
										/>
									),
								},
								{
									id: "tags",
									label: "Tags",
									content: (
										<ConsoleTags
											tags={tags.data ?? resource?.tags ?? {}}
											enabled={enabled && !!tags.data && !!data.LogGroupName}
											onChanged={() => refresh("Log group tags updated.")}
											plan={(next, removed) => [
												...(removed.length
													? [
															{
																label: "Remove tags",
																run: () =>
																	logsCall(scope, "UntagResource", {
																		ResourceArn: logsArn(scope),
																		TagKeys: removed,
																	}),
															},
														]
													: []),
												...(Object.keys(next).length
													? [
															{
																label: "Save tags",
																run: () =>
																	logsCall(scope, "TagResource", {
																		ResourceArn: logsArn(scope),
																		Tags: next,
																	}),
															},
														]
													: []),
											]}
										/>
									),
								},
							]}
						/>
					</SpaceBetween>
				)}
				{dialog && (
					<ActionDialog
						title={
							dialog === "delete"
								? "Delete log group"
								: dialog === "retention"
									? "Edit retention"
									: dialog === "create-stream"
										? "Create log stream"
										: dialog === "delete-stream"
											? "Delete log stream"
											: "Add log event"
						}
						submit={
							dialog === "delete"
								? "Delete log group"
								: dialog === "delete-stream"
									? "Delete log stream"
									: dialog === "create-stream"
										? "Create log stream"
										: dialog === "write"
											? "Add log event"
											: "Save changes"
						}
						confirmation={
							dialog === "delete"
								? scope.group
								: dialog === "delete-stream"
									? scope.stream
									: undefined
						}
						onClose={() => setDialog(null)}
						onSaved={() => {
							setDialog(null);
							if (dialog === "write") setEventsVersion((v) => v + 1);
							if (dialog === "delete") onDeleted();
							else if (dialog === "delete-stream") onStreamDeleted();
							else
								refresh(
									dialog === "write"
										? "Log event added."
										: dialog === "create-stream"
											? "Log stream created."
											: "Retention updated.",
								);
						}}
						plan={async () => {
							if (dialog === "delete")
								return [
									{
										label: "Delete log group",
										run: () => groupCall(scope, "DeleteLogGroup"),
									},
								];
							if (dialog === "delete-stream")
								return [
									{
										label: "Delete log stream",
										run: () =>
											groupCall(scope, "DeleteLogStream", {
												LogStreamName: scope.stream,
											}),
									},
								];
							if (dialog === "retention")
								return [
									{
										label: "Update retention",
										run: () =>
											groupCall(
												scope,
												retention === "0"
													? "DeleteRetentionPolicy"
													: "PutRetentionPolicy",
												retention === "0"
													? {}
													: {
															RetentionInDays: integer(
																retention,
																"Retention",
																1,
																3653,
															),
														},
											),
									},
								];
							if (dialog === "create-stream") {
								if (
									!streamName ||
									new TextEncoder().encode(streamName).length > 512 ||
									/[:*]/.test(streamName)
								)
									throw new Error(
										"Enter a stream name up to 512 bytes without : or *.",
									);
								return [
									{
										label: "Create stream",
										run: () =>
											groupCall(scope, "CreateLogStream", {
												LogStreamName: streamName,
											}),
									},
								];
							}
							if (
								!message ||
								new TextEncoder().encode(message).length > 1048550
							)
								throw new Error(
									"Enter a nonempty message, up to 1 MiB minus 26 bytes.",
								);
							const at = logTime(timestamp, "Event timestamp") ?? Date.now();
							// One event per request: validate its age before writing so a rejection never becomes a success notice.
							if (
								at < Date.now() - 14 * 86400000 ||
								at > Date.now() + 7200000 ||
								(data.RetentionInDays &&
									at < Date.now() - data.RetentionInDays * 86400000)
							)
								throw new Error(
									"The timestamp falls outside retention or the ingestion window (14 days ago to 2 hours ahead).",
								);
							return [
								{
									label: "Append log event",
									run: async () => {
										const out = await groupCall<{
											RejectedLogEventsInfo?: unknown;
										}>(scope, "PutLogEvents", {
											LogStreamName: scope.stream,
											LogEvents: [{ Timestamp: at, Message: message }],
										});
										if (out.RejectedLogEventsInfo)
											throw new Error(
												"The event was rejected by the ingestion time or retention limit. Choose a current timestamp and retry.",
											);
									},
								},
							];
						}}
					>
						{dialog === "delete" || dialog === "delete-stream" ? (
							<Alert type="warning">
								Delete {dialog === "delete" ? scope.group : scope.stream} and
								all its log events. This cannot be undone.
							</Alert>
						) : dialog === "retention" ? (
							<SpaceBetween size="l">
								<FormField label="Retention">
									<Select
										selectedOption={{
											value: retention,
											label:
												retention === "0"
													? "Never expire"
													: `${retention} days`,
										}}
										options={[
											{ value: "0", label: "Never expire" },
											...retentionDays.map((d) => ({
												value: String(d),
												label: `${d} days`,
											})),
										]}
										onChange={({ detail }) =>
											setRetention(detail.selectedOption.value ?? "0")
										}
									/>
								</FormField>
								<Alert type="warning">
									Reducing retention hides expired events immediately. A later
									write removes expired events from storage.
								</Alert>
							</SpaceBetween>
						) : dialog === "create-stream" ? (
							<FormField label="Log stream name">
								<Input
									ariaLabel="New log stream name"
									value={streamName}
									onChange={({ detail }) => setStreamName(detail.value)}
								/>
							</FormField>
						) : (
							<SpaceBetween size="l">
								<FormField label="Message">
									<Textarea
										ariaLabel="Log event message"
										rows={8}
										value={message}
										onChange={({ detail }) => setMessage(detail.value)}
									/>
								</FormField>
								<FormField
									label="Timestamp — optional"
									description="ISO 8601. Defaults to now."
								>
									<Input
										ariaLabel="Log event timestamp"
										value={timestamp}
										onChange={({ detail }) => setTimestamp(detail.value)}
									/>
								</FormField>
								<Alert type="info">
									Append one event. Attached subscription and metric filters
									also process it.
								</Alert>
							</SpaceBetween>
						)}
					</ActionDialog>
				)}
			</SpaceBetween>
		</ContentLayout>
	);
}
