import {
	Alert,
	Box,
	Button,
	Container,
	FormField,
	Header,
	Input,
	Link,
	Select,
	SpaceBetween,
	StatusIndicator,
	Table,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import type { Resource } from "#/lib/api";
import { integer } from "#/lib/consoleActions";
import {
	type MoveTask,
	redriveTarget,
	type SQSScope,
	sqsCall,
	sqsHref,
} from "#/lib/sqs";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";

export function SQSRedrive({
	scope,
	queues,
	arn,
	fifo,
	enabled,
	onChanged,
}: {
	scope: SQSScope;
	queues: Resource[];
	arn: string;
	fifo: boolean;
	enabled: boolean;
	onChanged: (message: string) => void;
}) {
	const follow = useFollow();
	const sources = queues.filter((q) => redriveTarget(q.details) === arn);
	const tasks = usePoll(
		async () =>
			enabled
				? ((
						await sqsCall<{ Results?: MoveTask[] }>(
							scope,
							"ListMessageMoveTasks",
							{ SourceArn: arn, MaxResults: 10 },
						)
					).Results ?? [])
				: [],
		5000,
	);
	const [dialog, setDialog] = useState<"start" | "cancel" | null>(null);
	const [destination, setDestination] = useState("");
	const [rate, setRate] = useState("500");
	const [selected, setSelected] = useState<MoveTask[]>([]);
	const target = selected[0];
	const destinations = queues
		.filter((q) => q.arn !== arn && q.details?.FifoQueue !== "true")
		.map((q) => ({ label: q.name, value: q.arn }));
	return (
		<SpaceBetween size="l">
			<Container
				header={<Header variant="h2">Dead-letter source queues</Header>}
			>
				<SpaceBetween size="m">
					<Box>Queues configured to send failed messages to this queue.</Box>
					{sources.length ? (
						sources.map((q) => (
							<Link
								key={q.arn}
								href={sqsHref({ ...scope, queue: q.name })}
								onFollow={follow}
							>
								{q.name}
							</Link>
						))
					) : (
						<Box color="text-body-secondary">
							This queue is not currently configured as a dead-letter queue.
						</Box>
					)}
				</SpaceBetween>
			</Container>
			{fifo && (
				<Alert type="info">
					TinyStack supports message redrive tasks for standard queues. FIFO
					queues still support dead-letter delivery.
				</Alert>
			)}
			{!!tasks.error && (
				<Alert type="error" header="Cannot load redrive tasks">
					{tasks.error}
				</Alert>
			)}
			<Table
				items={tasks.data ?? []}
				loading={tasks.loading}
				loadingText="Loading redrive tasks"
				trackBy="StartedTimestamp"
				selectionType="single"
				selectedItems={selected}
				onSelectionChange={({ detail }) => setSelected(detail.selectedItems)}
				ariaLabels={{
					selectionGroupLabel: "Select redrive task",
					itemSelectionLabel: (_, t) =>
						`Select task started ${new Date(t.StartedTimestamp).toLocaleString()}`,
				}}
				header={
					<Header
						variant="h2"
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									iconName="refresh"
									ariaLabel="Refresh redrive tasks"
									onClick={tasks.reload}
								/>
								<Button
									disabled={
										!enabled ||
										target?.Status !== "RUNNING" ||
										!target.TaskHandle
									}
									onClick={() => setDialog("cancel")}
								>
									Cancel redrive
								</Button>
								<Button
									variant="primary"
									disabled={
										!enabled ||
										fifo ||
										sources.length === 0 ||
										(tasks.data ?? []).some((t) => t.Status === "RUNNING")
									}
									onClick={() => {
										setDestination("");
										setRate("500");
										setDialog("start");
									}}
								>
									Start DLQ redrive
								</Button>
							</SpaceBetween>
						}
					>
						Message redrive tasks
					</Header>
				}
				columnDefinitions={[
					{
						id: "started",
						header: "Started",
						cell: (t) => new Date(t.StartedTimestamp).toLocaleString(),
					},
					{
						id: "status",
						header: "Status",
						cell: (t) => (
							<StatusIndicator
								type={
									t.Status === "FAILED"
										? "error"
										: t.Status === "RUNNING"
											? "in-progress"
											: t.Status === "COMPLETED"
												? "success"
												: "stopped"
								}
							>
								{t.Status}
							</StatusIndicator>
						),
					},
					{
						id: "destination",
						header: "Destination",
						cell: (t) => t.DestinationArn ?? "Original source queues",
					},
					{
						id: "moved",
						header: "Messages moved",
						cell: (t) => t.ApproximateNumberOfMessagesMoved,
					},
					{
						id: "total",
						header: "Messages to move",
						cell: (t) => t.ApproximateNumberOfMessagesToMove,
					},
					{
						id: "failure",
						header: "Failure reason",
						cell: (t) => t.FailureReason ?? "—",
					},
				]}
				empty={
					<Box textAlign="center" padding="l">
						No message redrive tasks
					</Box>
				}
			/>
			{dialog && (
				<ActionDialog
					title={
						dialog === "start"
							? "Start dead-letter queue redrive"
							: "Cancel message redrive"
					}
					submit={dialog === "start" ? "Start redrive" : "Cancel redrive"}
					onClose={() => setDialog(null)}
					onSaved={() => {
						setDialog(null);
						setSelected([]);
						tasks.reload();
						onChanged(
							dialog === "start"
								? "Message redrive started."
								: "Message redrive cancelled.",
						);
					}}
					plan={async () => {
						if (dialog === "cancel")
							return [
								{
									label: "Cancel redrive",
									run: () =>
										sqsCall(scope, "CancelMessageMoveTask", {
											TaskHandle: target?.TaskHandle,
										}),
								},
							];
						const limit = integer(rate, "Maximum messages per second", 1, 500);
						return [
							{
								label: "Start redrive",
								run: () =>
									sqsCall(scope, "StartMessageMoveTask", {
										SourceArn: arn,
										MaxNumberOfMessagesPerSecond: limit,
										...(destination ? { DestinationArn: destination } : {}),
									}),
							},
						];
					}}
				>
					{dialog === "cancel" ? (
						<Alert type="warning">
							Messages already moved remain at their destinations. Cancelling
							stops further movement.
						</Alert>
					) : (
						<SpaceBetween size="l">
							<Alert type="info">
								Move visible messages from this dead-letter queue. Redriving to
								the original source only moves messages that arrived through
								dead-letter delivery.
							</Alert>
							<FormField label="Destination">
								<Select
									ariaLabel="Redrive destination"
									selectedOption={
										destinations.find((d) => d.value === destination) ?? {
											label: "Original source queues",
											value: "",
										}
									}
									options={[
										{ label: "Original source queues", value: "" },
										...destinations,
									]}
									onChange={({ detail }) =>
										setDestination(detail.selectedOption.value ?? "")
									}
								/>
							</FormField>
							<FormField
								label="Maximum messages per second"
								constraintText="1–500"
							>
								<Input
									ariaLabel="Maximum messages per second"
									type="number"
									value={rate}
									onChange={({ detail }) => setRate(detail.value)}
								/>
							</FormField>
						</SpaceBetween>
					)}
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
