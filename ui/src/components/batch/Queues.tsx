import { Alert, Link, SpaceBetween } from "@cloudscape-design/components";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Copy } from "#/components/efs/Shared";
import { Details } from "#/components/elbv2/Shared";
import { batchCall, queues } from "#/lib/batch";
import {
	batchHref,
	environmentNames,
	type JobQueue,
	pending,
} from "#/lib/batchHelpers";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { CreateQueueDialog, EditQueueDialog } from "./QueueDialog";
import { DetailTabs, ResourceTags, type SectionProps, Status } from "./Shared";

type Dialog = "" | "create" | "edit" | "delete";

export function Queues({
	scope,
	enabled,
	selected,
	tab,
	onChanged,
}: SectionProps) {
	const navigate = useNavigate(),
		follow = useFollow();
	const [changing, setChanging] = useState(true),
		[dialog, setDialog] = useState<Dialog>("");
	const poll = usePoll(
		async () => {
			const rows = enabled ? await queues(scope) : [];
			setChanging(rows.some((q) => pending(q.Status)));
			return rows;
		},
		changing ? 2000 : 0,
	);
	const q = poll.data?.find((q) => q.JobQueueName === selected);
	const saved = () => {
		setDialog("");
		setChanging(true);
		poll.reload();
		onChanged();
	};
	const list = batchHref(scope, "queues");
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{!selected ? (
				<Rows
					title="Job queues"
					items={poll.data ?? []}
					loading={poll.loading}
					emptyText="Choose Create job queue to add one."
					actions={
						<Buttons>
							<Action onClick={poll.reload}>Refresh</Action>
							<Action disabled={!enabled} onClick={() => setDialog("create")}>
								Create job queue
							</Action>
						</Buttons>
					}
					columns={[
						{
							id: "name",
							header: "Name",
							cell: (q) => (
								<Link
									href={batchHref(scope, "queues", { queue: q.JobQueueName })}
									onFollow={follow}
								>
									{q.JobQueueName}
								</Link>
							),
						},
						{
							id: "state",
							header: "State",
							cell: (q) => <Status value={q.State} />,
						},
						{
							id: "status",
							header: "Status",
							cell: (q) => <Status value={q.Status} />,
						},
						{
							id: "priority",
							header: "Priority",
							cell: (q) => q.Priority ?? "-",
						},
						{
							id: "environments",
							header: "Compute environments",
							cell: (q) =>
								environmentNames(q.ComputeEnvironmentOrder).join(", ") || "-",
						},
					]}
				/>
			) : q ? (
				<SpaceBetween size="m">
					<Buttons>
						<Action onClick={() => navigate({ href: list })}>Job queues</Action>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action
							onClick={() =>
								navigate({
									href: batchHref(scope, "jobs", { queue: q.JobQueueName }),
								})
							}
						>
							View jobs
						</Action>
						<Action
							disabled={!enabled || pending(q.Status)}
							onClick={() => setDialog("edit")}
						>
							Edit
						</Action>
						<Action
							disabled={!enabled || pending(q.Status)}
							onClick={() => setDialog("delete")}
						>
							Delete
						</Action>
					</Buttons>
					<DetailTabs
						tab={tab}
						href={(next) =>
							batchHref(scope, "queues", { queue: selected, tab: next })
						}
						overview={<Overview queue={q} />}
						tags={
							<ResourceTags
								scope={scope}
								arn={q.JobQueueArn}
								tags={q.Tags}
								enabled={enabled && !pending(q.Status)}
								onChanged={saved}
							/>
						}
					/>
				</SpaceBetween>
			) : (
				!poll.loading && (
					<Alert>Job queue not found in this account and Region.</Alert>
				)
			)}
			{dialog === "create" && (
				<CreateQueueDialog
					scope={scope}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
			{dialog === "edit" && !!q && (
				<EditQueueDialog
					scope={scope}
					queue={q}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
			{dialog === "delete" && !!q && (
				<ActionDialog
					title="Delete job queue"
					submit="Delete job queue"
					confirmation={q.JobQueueName}
					onClose={() => setDialog("")}
					onSaved={() => {
						saved();
						navigate({ href: list });
					}}
					plan={async () => [
						{
							label: "DeleteJobQueue",
							run: () =>
								batchCall(scope, "DeleteJobQueue", {
									JobQueue: q.JobQueueName,
								}),
						},
					]}
				>
					<Alert type="warning">
						Delete {q.JobQueueName} permanently. Disable it first.
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}

function Overview({ queue: q }: { queue: JobQueue }) {
	return (
		<Details
			items={[
				["Name", q.JobQueueName],
				["ARN", <Copy key="arn" text={q.JobQueueArn} label="ARN" />],
				["State", <Status key="state" value={q.State} />],
				["Status", <Status key="status" value={q.Status} />],
				["Status reason", q.StatusReason],
				["Priority", q.Priority === undefined ? "-" : String(q.Priority)],
				[
					"Compute environments (in order)",
					environmentNames(q.ComputeEnvironmentOrder).join(", "),
				],
			]}
		/>
	);
}
