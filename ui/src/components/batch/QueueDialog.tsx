import {
	Alert,
	Button,
	FormField,
	Select,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Choice, Field } from "#/components/containers/Controls";
import { batchCall, environments } from "#/lib/batch";
import {
	type BatchScope,
	emptyQueueForm,
	environmentNames,
	type JobQueue,
	type QueueForm,
	queueInput,
	queueUpdate,
	states,
} from "#/lib/batchHelpers";
import { usePoll } from "#/lib/usePoll";

// EnvironmentRows chooses a queue's compute environments in order of preference.
function EnvironmentRows({
	scope,
	values,
	onChange,
}: {
	scope: BatchScope;
	values: string[];
	onChange: (values: string[]) => void;
}) {
	const poll = usePoll(() => environments(scope), 0);
	const names = (poll.data ?? []).map((c) => c.ComputeEnvironmentName);
	return (
		<FormField
			label="Compute environments"
			description="In order of preference: a job runs in the first environment with room for it. A queue takes up to three."
		>
			<SpaceBetween size="xs">
				{!!poll.error && <Alert type="error">{poll.error}</Alert>}
				{values.map((value, i) => (
					// biome-ignore lint/suspicious/noArrayIndexKey: the rows are an ordered list without identity.
					<SpaceBetween key={`${i}`} direction="horizontal" size="xs">
						<Select
							ariaLabel={`Compute environment ${i + 1}`}
							placeholder="Choose a compute environment"
							empty="No compute environments."
							selectedOption={value ? { label: value, value } : null}
							options={names
								.filter((n) => n === value || !values.includes(n))
								.map((n) => ({ label: n, value: n }))}
							onChange={({ detail }) =>
								onChange(
									values.map((v, j) =>
										j === i ? (detail.selectedOption.value ?? "") : v,
									),
								)
							}
						/>
						<Button
							ariaLabel={`Remove compute environment ${i + 1}`}
							onClick={() => onChange(values.filter((_, j) => j !== i))}
						>
							Remove
						</Button>
					</SpaceBetween>
				))}
				<Button
					disabled={values.length >= 3}
					onClick={() => onChange([...values, ""])}
				>
					Add compute environment
				</Button>
			</SpaceBetween>
		</FormField>
	);
}

export function CreateQueueDialog({
	scope,
	onClose,
	onSaved,
}: {
	scope: BatchScope;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [f, setF] = useState(emptyQueueForm);
	const set = (p: Partial<QueueForm>) => setF((v) => ({ ...v, ...p }));
	return (
		<ActionDialog
			title="Create job queue"
			submit="Create job queue"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = queueInput(f);
				return [
					{
						label: "CreateJobQueue",
						run: () => batchCall(scope, "CreateJobQueue", input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Field label="Name" value={f.name} onChange={(name) => set({ name })} />
				<Choice
					label="State"
					value={f.state}
					options={states}
					onChange={(state) => set({ state })}
				/>
				<Field
					label="Priority"
					value={f.priority}
					onChange={(priority) => set({ priority })}
				/>
				<EnvironmentRows
					scope={scope}
					values={f.environments}
					onChange={(environments) => set({ environments })}
				/>
				<Field
					label="Tags (JSON string map)"
					value={f.tags}
					onChange={(tags) => set({ tags })}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}

export function EditQueueDialog({
	scope,
	queue: q,
	onClose,
	onSaved,
}: {
	scope: BatchScope;
	queue: JobQueue;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [f, setF] = useState({
		state: q.State ?? "ENABLED",
		priority: String(q.Priority ?? 0),
		environments: environmentNames(q.ComputeEnvironmentOrder),
	});
	return (
		<ActionDialog
			title={`Edit ${q.JobQueueName}`}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = queueUpdate(q, f);
				return [
					{
						label: "UpdateJobQueue",
						run: () => batchCall(scope, "UpdateJobQueue", input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Choice
					label="State"
					value={f.state}
					options={states}
					onChange={(state) => setF((v) => ({ ...v, state }))}
				/>
				<Field
					label="Priority"
					value={f.priority}
					onChange={(priority) => setF((v) => ({ ...v, priority }))}
				/>
				<EnvironmentRows
					scope={scope}
					values={f.environments}
					onChange={(environments) => setF((v) => ({ ...v, environments }))}
				/>
				<Alert>
					Remove every compute environment to take the queue off all of them.
				</Alert>
			</SpaceBetween>
		</ActionDialog>
	);
}
