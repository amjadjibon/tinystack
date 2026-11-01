import {
	FormField,
	Input,
	Select,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { type Parameter, type SSMScope, ssmCall } from "#/lib/ssm";
export const parameterTypes = ["String", "StringList", "SecureString"];
export const parameterTiers = ["Standard", "Advanced", "Intelligent-Tiering"];
export function Choice({
	label,
	value,
	options,
	onChange,
}: {
	label: string;
	value: string;
	options: string[];
	onChange: (v: string) => void;
}) {
	return (
		<FormField label={label}>
			<Select
				ariaLabel={label}
				selectedOption={{ label: value || "All", value }}
				options={options.map((value) => ({ label: value || "All", value }))}
				onChange={({ detail }) => onChange(detail.selectedOption.value ?? "")}
			/>
		</FormField>
	);
}
export function ParameterForm({
	scope,
	parameter,
	onClose,
	onSaved,
}: {
	scope: SSMScope;
	parameter?: Parameter;
	onClose: () => void;
	onSaved: (name: string) => void;
}) {
	const [name, setName] = useState(parameter?.Name ?? ""),
		[value, setValue] = useState(""),
		[type, setType] = useState(parameter?.Type ?? "String"),
		[tier, setTier] = useState(parameter?.Tier ?? "Standard"),
		[key, setKey] = useState(parameter?.KeyId ?? ""),
		[description, setDescription] = useState(parameter?.Description ?? ""),
		[pattern, setPattern] = useState(parameter?.AllowedPattern ?? ""),
		[dataType, setDataType] = useState("text");
	return (
		<ActionDialog
			title={parameter ? "Overwrite parameter" : "Create parameter"}
			submit={parameter ? "Overwrite parameter" : "Create parameter"}
			confirmation={parameter?.Name}
			onClose={onClose}
			onSaved={() => onSaved(name)}
			plan={async () => {
				if (!name.trim() || !value)
					throw new Error("Enter a parameter name and a new value.");
				return [
					{
						label: parameter ? "Overwrite parameter" : "Create parameter",
						run: () =>
							ssmCall(scope, "PutParameter", {
								Name: name,
								Value: value,
								Type: type,
								Tier: tier,
								Description: description,
								AllowedPattern: pattern,
								DataType: dataType,
								Overwrite: !!parameter,
								...(type === "SecureString" && key ? { KeyId: key } : {}),
							}),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<FormField label="Name">
					<Input
						ariaLabel="Parameter name"
						value={name}
						disabled={!!parameter}
						onChange={({ detail }) => setName(detail.value)}
					/>
				</FormField>
				<Choice
					label="Type"
					value={type}
					options={parameterTypes}
					onChange={setType}
				/>
				<FormField
					label="Value"
					description="Enter a new value. StringList values use commas as separators."
				>
					<Input
						ariaLabel="Parameter value"
						type="password"
						value={value}
						onChange={({ detail }) => setValue(detail.value)}
					/>
				</FormField>
				<Choice
					label="Tier"
					value={tier}
					options={parameterTiers}
					onChange={setTier}
				/>
				{type === "SecureString" && (
					<FormField
						label="KMS key ID"
						description="Leave blank for the current key or alias/aws/ssm."
					>
						<Input
							ariaLabel="KMS key ID"
							value={key}
							onChange={({ detail }) => setKey(detail.value)}
						/>
					</FormField>
				)}
				<FormField
					label="Description"
					description={
						parameter
							? "An empty description retains the current description."
							: undefined
					}
				>
					<Textarea
						ariaLabel="Description"
						value={description}
						onChange={({ detail }) => setDescription(detail.value)}
					/>
				</FormField>
				<FormField
					label="Allowed pattern"
					description={
						parameter
							? "An empty pattern retains the current pattern."
							: undefined
					}
				>
					<Input
						ariaLabel="Allowed pattern"
						value={pattern}
						onChange={({ detail }) => setPattern(detail.value)}
					/>
				</FormField>
				<Choice
					label="Data type"
					value={dataType}
					options={["text"]}
					onChange={setDataType}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}
