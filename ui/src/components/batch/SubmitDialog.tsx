import {
	Alert,
	Checkbox,
	ExpandableSection,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Field } from "#/components/containers/Controls";
import { Pick, Text } from "#/components/memorydb/Fields";
import { batchCall, definitions, queues } from "#/lib/batch";
import {
	type BatchScope,
	definitionKey,
	emptySubmitForm,
	resources,
	type SubmitForm,
	submitInput,
} from "#/lib/batchHelpers";
import { usePoll } from "#/lib/usePoll";

// SubmitDialog submits a job to an enabled queue from an active definition.
export function SubmitDialog({
	scope,
	queue,
	definition,
	onClose,
	onSaved,
}: {
	scope: BatchScope;
	queue?: string;
	// name:revision of the definition to start from.
	definition?: string;
	onClose: () => void;
	onSaved: (jobId: string, queue: string) => void;
}) {
	const [f, setF] = useState(() => emptySubmitForm(queue, definition));
	const set = (p: Partial<SubmitForm>) => setF((v) => ({ ...v, ...p }));
	const options = usePoll(async () => {
		const [queueList, definitionList] = await Promise.all([
			queues(scope),
			definitions(scope, "ACTIVE"),
		]);
		return {
			queues: queueList.filter(
				(q) => q.State === "ENABLED" && q.Status === "VALID",
			),
			definitions: definitionList,
		};
	}, 0);
	const submitted = useRef("");
	const chosen = options.data?.definitions.find(
		(d) => definitionKey(d) === f.definition,
	);
	const current = resources(chosen?.ContainerProperties);
	return (
		<ActionDialog
			title="Submit job"
			submit="Submit job"
			onClose={onClose}
			onSaved={() => onSaved(submitted.current, f.queue)}
			plan={async () => {
				const input = submitInput(f, options.data?.definitions ?? []);
				return [
					{
						label: "SubmitJob",
						run: async () => {
							const out = await batchCall<{ JobId: string }>(
								scope,
								"SubmitJob",
								input,
							);
							submitted.current = out.JobId;
						},
					},
				];
			}}
		>
			<SpaceBetween size="m">
				{!!options.error && <Alert type="error">{options.error}</Alert>}
				{!!options.data && !options.data.queues.length && (
					<Alert type="warning">
						No job queue accepts jobs. Create or enable a job queue first.
					</Alert>
				)}
				<Text
					label="Job name"
					value={f.name}
					onChange={(name) => set({ name })}
				/>
				<Pick
					label="Job queue"
					placeholder="Choose a job queue"
					value={f.queue}
					options={(options.data?.queues ?? []).map((q) => ({
						value: q.JobQueueName,
					}))}
					onChange={(queue) => set({ queue })}
				/>
				<Pick
					label="Job definition"
					placeholder="Choose an active job definition"
					value={f.definition}
					options={(options.data?.definitions ?? []).map((d) => ({
						value: definitionKey(d),
					}))}
					onChange={(definition) => set({ definition })}
				/>
				<Field
					label="Parameters (JSON string map)"
					value={f.parameters}
					onChange={(parameters) => set({ parameters })}
				/>
				<ExpandableSection headerText="Overrides, array and dependencies">
					<SpaceBetween size="m">
						<Text
							label="Command (JSON array of strings, optional)"
							value={f.command}
							onChange={(command) => set({ command })}
						/>
						<Field
							label="Environment variables (JSON string map)"
							value={f.environment}
							onChange={(environment) => set({ environment })}
						/>
						<Text
							label="vCPUs (optional)"
							description={
								chosen
									? `The definition sets ${current.vcpus ?? "-"} vCPUs and ${current.memory ?? "-"} MiB. Leave a value blank to keep it.`
									: "Leave blank to keep the definition's value."
							}
							value={f.vcpus}
							onChange={(vcpus) => set({ vcpus })}
						/>
						<Text
							label="Memory in MiB (optional)"
							value={f.memory}
							onChange={(memory) => set({ memory })}
						/>
						<Text
							label="Retry attempts (1-10, optional)"
							value={f.attempts}
							onChange={(attempts) => set({ attempts })}
						/>
						<Text
							label="Timeout in seconds (at least 60, optional)"
							value={f.timeout}
							onChange={(timeout) => set({ timeout })}
						/>
						<Text
							label="Array size (2-10000, optional)"
							value={f.size}
							onChange={(size) => set({ size })}
						/>
						<Checkbox
							checked={f.sequential}
							onChange={({ detail }) => set({ sequential: detail.checked })}
						>
							Run array children one after another
						</Checkbox>
						<Text
							label="Depends on (job IDs, comma separated, optional)"
							value={f.depends}
							onChange={(depends) => set({ depends })}
						/>
					</SpaceBetween>
				</ExpandableSection>
				<Field
					label="Tags (JSON string map)"
					value={f.tags}
					onChange={(tags) => set({ tags })}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}
