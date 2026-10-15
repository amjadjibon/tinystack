import {
	FormField,
	Input,
	Select,
	Textarea,
} from "@cloudscape-design/components";
export function CostInput({
	label,
	value,
	onChange,
	disabled = false,
}: {
	label: string;
	value: string;
	onChange: (value: string) => void;
	disabled?: boolean;
}) {
	return (
		<FormField label={label}>
			<Input
				ariaLabel={label}
				value={value}
				disabled={disabled}
				onChange={({ detail }) => onChange(detail.value)}
			/>
		</FormField>
	);
}
export function CostSelect({
	label,
	value,
	options,
	onChange,
}: {
	label: string;
	value: string;
	options: string[];
	onChange: (value: string) => void;
}) {
	return (
		<FormField label={label}>
			<Select
				ariaLabel={label}
				selectedOption={{ label: value, value }}
				options={options.map((value) => ({ label: value, value }))}
				onChange={({ detail }) => onChange(detail.selectedOption.value ?? "")}
			/>
		</FormField>
	);
}
export function CostJSON({
	label,
	value,
	onChange,
	description,
}: {
	label: string;
	value: string;
	onChange: (value: string) => void;
	description?: string;
}) {
	return (
		<FormField label={label} description={description}>
			<Textarea
				ariaLabel={label}
				value={value}
				rows={8}
				onChange={({ detail }) => onChange(detail.value)}
			/>
		</FormField>
	);
}
