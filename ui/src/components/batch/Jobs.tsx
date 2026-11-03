import {
	Alert,
	ColumnLayout,
	FormField,
	Link,
	Select,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { Action, Buttons } from "#/components/containers/Controls";
import { jobs, queues } from "#/lib/batch";
import {
	allStatuses,
	batchHref,
	jobPending,
	jobStatuses,
} from "#/lib/batchHelpers";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { JobDetails } from "./JobDetails";
import { JobRows } from "./JobRows";
import type { SectionProps } from "./Shared";
import { SubmitDialog } from "./SubmitDialog";

type JobsProps = SectionProps & {
	// The queue whose jobs are listed, and their status or ALL.
	queue: string;
	status: string;
};

const statusOptions = [allStatuses, ...jobStatuses].map((value) => ({
	value,
	label: value === allStatuses ? "All statuses" : value,
}));

export function Jobs(props: JobsProps) {
	const navigate = useNavigate();
	const { scope, queue, status } = props;
	if (props.selected)
		return (
			<JobDetails
				key={props.selected}
				scope={scope}
				enabled={props.enabled}
				docker={props.docker}
				id={props.selected}
				queue={queue}
				status={status}
				tab={props.tab}
				onBack={() =>
					navigate({ href: batchHref(scope, "jobs", { queue, status }) })
				}
				onChanged={props.onChanged}
			/>
		);
	return <JobList {...props} />;
}

function JobList({
	scope,
	enabled,
	docker,
	queue,
	status,
	onChanged,
}: JobsProps) {
	const navigate = useNavigate(),
		follow = useFollow();
	const poll = usePoll(
		() => (enabled ? queues(scope) : Promise.resolve([])),
		0,
	);
	const names = (poll.data ?? []).map((q) => q.JobQueueName);
	// Without a queue in the address the list shows the first queue.
	const name = queue || names[0] || "";
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<ColumnLayout columns={2}>
				<FormField label="Job queue">
					<Select
						ariaLabel="Job queue"
						placeholder="Choose a job queue"
						empty="No job queues."
						selectedOption={name ? { label: name, value: name } : null}
						options={names.map((value) => ({ value, label: value }))}
						onChange={({ detail }) =>
							navigate({
								href: batchHref(scope, "jobs", {
									queue: detail.selectedOption.value,
									status,
								}),
							})
						}
					/>
				</FormField>
				<FormField label="Status">
					<Select
						ariaLabel="Status"
						selectedOption={
							statusOptions.find((o) => o.value === status) ?? statusOptions[0]
						}
						options={statusOptions}
						onChange={({ detail }) =>
							navigate({
								href: batchHref(scope, "jobs", {
									queue: name,
									status: detail.selectedOption.value,
								}),
							})
						}
					/>
				</FormField>
			</ColumnLayout>
			{names.includes(name) ? (
				<JobTable
					key={`${name}/${status}`}
					scope={scope}
					enabled={enabled}
					docker={docker}
					queue={name}
					status={status}
					onChanged={onChanged}
				/>
			) : (
				!poll.loading && (
					<Alert>
						{name
							? `Job queue ${name} was not found in this account and Region.`
							: "This account and Region have no job queues."}{" "}
						<Link href={batchHref(scope, "queues")} onFollow={follow}>
							Go to job queues
						</Link>
					</Alert>
				)
			)}
		</SpaceBetween>
	);
}

function JobTable({
	scope,
	enabled,
	docker,
	queue,
	status,
	onChanged,
}: Pick<JobsProps, "scope" | "enabled" | "docker" | "onChanged"> & {
	queue: string;
	status: string;
}) {
	const navigate = useNavigate();
	const [changing, setChanging] = useState(true),
		[submitting, setSubmitting] = useState(false);
	const poll = usePoll(
		async () => {
			const rows = enabled ? await jobs(scope, queue, status) : [];
			setChanging(rows.some((j) => jobPending(j.Status, docker)));
			return rows;
		},
		changing ? 2000 : 0,
	);
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<JobRows
				title="Jobs"
				items={poll.data ?? []}
				loading={poll.loading}
				emptyText={
					status === allStatuses
						? "Choose Submit job to add one."
						: "Choose another status or Submit job."
				}
				href={(j) => batchHref(scope, "jobs", { queue, status, job: j.JobId })}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action disabled={!enabled} onClick={() => setSubmitting(true)}>
							Submit job
						</Action>
					</Buttons>
				}
			/>
			{submitting && (
				<SubmitDialog
					scope={scope}
					queue={queue}
					onClose={() => setSubmitting(false)}
					onSaved={(job, submitted) => {
						setSubmitting(false);
						onChanged();
						navigate({
							href: batchHref(scope, "jobs", { queue: submitted, status, job }),
						});
					}}
				/>
			)}
		</SpaceBetween>
	);
}
