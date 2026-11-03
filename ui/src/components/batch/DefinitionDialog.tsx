import {
	Alert,
	FormField,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Field } from "#/components/containers/Controls";
import { Text } from "#/components/memorydb/Fields";
import { batchCall } from "#/lib/batch";
import {
	type BatchScope,
	type DefinitionForm,
	definitionForm,
	definitionInput,
	type JobDefinition,
} from "#/lib/batchHelpers";

function Json({
	label,
	description,
	value,
	rows,
	onChange,
}: {
	label: string;
	description?: string;
	value: string;
	rows: number;
	onChange: (value: string) => void;
}) {
	return (
		<FormField label={label} description={description}>
			<Textarea
				ariaLabel={label}
				rows={rows}
				value={value}
				onChange={({ detail }) => onChange(detail.value)}
			/>
		</FormField>
	);
}

// DefinitionDialog registers a job definition, or the next revision of one
// when it starts from an existing definition.
export function DefinitionDialog({
	scope,
	from,
	onClose,
	onSaved,
}: {
	scope: BatchScope;
	from?: JobDefinition;
	onClose: () => void;
	// onSaved gets the new revision as name:revision.
	onSaved: (revision: string) => void;
}) {
	const [f, setF] = useState(() => definitionForm(from));
	const set = (p: Partial<DefinitionForm>) => setF((v) => ({ ...v, ...p }));
	const registered = useRef("");
	return (
		<ActionDialog
			title={
				from
					? `Register a revision of ${from.JobDefinitionName}`
					: "Register job definition"
			}
			submit="Register job definition"
			onClose={onClose}
			onSaved={() => onSaved(registered.current)}
			plan={async () => {
				const input = definitionInput(f);
				return [
					{
						label: "RegisterJobDefinition",
						run: async () => {
							const out = await batchCall<{
								JobDefinitionName: string;
								Revision: number;
							}>(scope, "RegisterJobDefinition", input);
							registered.current = `${out.JobDefinitionName}:${out.Revision}`;
						},
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Text
					label="Name"
					value={f.name}
					disabled={!!from}
					onChange={(name) => set({ name })}
				/>
				<Alert>
					Batch runs container jobs on the EC2 platform here. Registering a name
					again adds the next revision.
				</Alert>
				<Json
					label="Container properties (JSON)"
					description="Member names are not case sensitive: image or Image. Use Ref::name in the command for a parameter."
					rows={12}
					value={f.container}
					onChange={(container) => set({ container })}
				/>
				<Field
					label="Parameters (JSON string map)"
					value={f.parameters}
					onChange={(parameters) => set({ parameters })}
				/>
				<Field
					label="Retry attempts (1-10, optional)"
					value={f.attempts}
					onChange={(attempts) => set({ attempts })}
				/>
				<Json
					label="Retry rules (JSON array, optional)"
					description='Up to six evaluateOnExit rules, such as [{"OnExitCode": "137", "Action": "RETRY"}].'
					rows={4}
					value={f.rules}
					onChange={(rules) => set({ rules })}
				/>
				<Field
					label="Timeout in seconds (at least 60, optional)"
					value={f.timeout}
					onChange={(timeout) => set({ timeout })}
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
