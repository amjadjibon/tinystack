import {
	Alert,
	FormField,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Choice, Field } from "#/components/containers/Controls";
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
					Fargate requires an executionRoleArn and valid VCPU/MEMORY
					resourceRequirements. Set platform options below; choose a valid
					resource pair in the container JSON (for example 1 VCPU and 2048 MiB).
					Docker mode executes EC2 jobs only. Registering a name again adds the
					next revision.
				</Alert>
				<Choice
					label="Platform"
					value={f.platform}
					options={["EC2", "FARGATE"]}
					onChange={(platform) => set({ platform })}
				/>
				{f.platform === "FARGATE" && (
					<SpaceBetween size="m">
						<Field
							label="Execution role ARN"
							value={f.executionRole}
							onChange={(executionRole) => set({ executionRole })}
						/>
						<Choice
							label="Assign public IP"
							value={f.publicIp}
							options={["DISABLED", "ENABLED"]}
							onChange={(publicIp) => set({ publicIp })}
						/>
						<Choice
							label="Platform version"
							value={f.platformVersion}
							options={["LATEST", "1.4.0", "1.3.0", "1.0.0"]}
							onChange={(platformVersion) => set({ platformVersion })}
						/>
						<Choice
							label="CPU architecture"
							value={f.architecture}
							options={["X86_64", "ARM64"]}
							onChange={(architecture) => set({ architecture })}
						/>
						<Choice
							label="Operating system"
							value={f.os}
							options={[
								"LINUX",
								"WINDOWS_SERVER_2019_CORE",
								"WINDOWS_SERVER_2019_FULL",
								"WINDOWS_SERVER_2022_CORE",
								"WINDOWS_SERVER_2022_FULL",
							]}
							onChange={(os) => set({ os })}
						/>
					</SpaceBetween>
				)}

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
