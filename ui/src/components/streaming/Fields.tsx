import { FormField, Input, Select } from "@cloudscape-design/components";
export function Field({
	label,
	value,
	onChange,
	type = "text",
}: {
	label: string;
	value: string;
	onChange: (value: string) => void;
	type?: "text" | "number" | "password";
}) {
	return (
		<FormField label={label}>
			<Input
				ariaLabel={label}
				value={value}
				type={type}
				onChange={({ detail }) => onChange(detail.value)}
			/>
		</FormField>
	);
}
export function Choice({
	disabled = false,
	label,
	value,
	options,
	onChange,
}: {
	label: string;
	value: string;
	options: string[];
	disabled?: boolean;
	onChange: (value: string) => void;
}) {
	return (
		<FormField label={label}>
			<Select
				disabled={disabled}
				ariaLabel={label}
				selectedOption={{ label: value, value }}
				options={options.map((value) => ({ label: value, value }))}
				onChange={({ detail }) => onChange(detail.selectedOption.value ?? "")}
			/>
		</FormField>
	);
}
