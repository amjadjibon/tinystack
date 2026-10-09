import {
	Autosuggest,
	FormField,
	Input,
	Select,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import type { Resource } from "#/lib/api";
import { type LambdaScope, runtimes, type SettingsInput } from "#/lib/lambda";

export function LambdaSettings({
	value,
	onChange,
	scope,
	resources,
	image,
}: {
	value: SettingsInput;
	onChange: (value: SettingsInput) => void;
	scope: LambdaScope;
	resources: Resource[];
	image: boolean;
}) {
	const set = (change: Partial<SettingsInput>) =>
		onChange({ ...value, ...change });
	return (
		<SpaceBetween size="l">
			<FormField
				label="Description — optional"
				constraintText="Maximum 256 characters"
			>
				<Input
					ariaLabel="Function description"
					value={value.description}
					onChange={({ detail }) => set({ description: detail.value })}
				/>
			</FormField>
			{!image && (
				<>
					<FormField label="Runtime">
						<Select
							selectedOption={{ value: value.runtime, label: value.runtime }}
							options={runtimes.map((r) => ({ value: r, label: r }))}
							onChange={({ detail }) =>
								set({ runtime: detail.selectedOption.value ?? "" })
							}
						/>
					</FormField>
					<FormField
						label="Handler"
						description="The entry point in your deployment package, for example lambda_function.lambda_handler for Python."
					>
						<Input
							ariaLabel="Function handler"
							value={value.handler}
							onChange={({ detail }) => set({ handler: detail.value })}
						/>
					</FormField>
				</>
			)}
			<FormField
				label="Execution role"
				description="Choose an existing IAM role or enter its ARN. Manage the role's trust and permissions in IAM."
			>
				<Autosuggest
					ariaLabel="Execution role ARN"
					value={value.role}
					onChange={({ detail }) => set({ role: detail.value })}
					options={resources
						.filter(
							(r) =>
								r.service === "iam" &&
								r.type === "role" &&
								r.account === scope.account,
						)
						.map((r) => ({ value: r.arn, label: r.name, description: r.arn }))}
				/>
			</FormField>
			<FormField label="Memory (MB)" constraintText="128–10,240 MB">
				<Input
					ariaLabel="Function memory"
					type="number"
					value={value.memory}
					onChange={({ detail }) => set({ memory: detail.value })}
				/>
			</FormField>
			<FormField label="Timeout (seconds)" constraintText="1–900 seconds">
				<Input
					ariaLabel="Function timeout"
					type="number"
					value={value.timeout}
					onChange={({ detail }) => set({ timeout: detail.value })}
				/>
			</FormField>
			<FormField
				label="Environment variables (JSON)"
				description='Object of string values, for example {"STAGE":"dev"}. An empty object removes all variables.'
				constraintText="Maximum 4 KiB; reserved AWS variables cannot be changed."
			>
				<Textarea
					ariaLabel="Environment variables"
					value={value.environment}
					rows={6}
					onChange={({ detail }) => set({ environment: detail.value })}
				/>
			</FormField>
		</SpaceBetween>
	);
}
