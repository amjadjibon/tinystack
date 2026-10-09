import {
	FormField,
	Input,
	Select,
	Textarea,
} from "@cloudscape-design/components";
export function GatewayInput({
	label,
	value,
	onChange,
	description,
	disabled = false,
}: {
	label: string;
	value: string;
	onChange: (v: string) => void;
	description?: string;
	disabled?: boolean;
}) {
	return (
		<FormField label={label} description={description}>
			<Input
				ariaLabel={label}
				value={value}
				disabled={disabled}
				onChange={({ detail }) => onChange(detail.value)}
			/>
		</FormField>
	);
}
export function GatewayText({
	label,
	value,
	onChange,
	description,
	rows = 5,
}: {
	label: string;
	value: string;
	onChange: (v: string) => void;
	description?: string;
	rows?: number;
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
export function GatewaySelect({
	label,
	value,
	onChange,
	options,
	description,
	placeholder,
}: {
	label: string;
	value: string;
	onChange: (v: string) => void;
	options: { label: string; value: string }[];
	description?: string;
	placeholder?: string;
}) {
	return (
		<FormField label={label} description={description}>
			<Select
				selectedOption={
					options.find((o) => o.value === value) ??
					(value ? { value, label: value } : null)
				}
				placeholder={placeholder ?? "Choose an option"}
				options={options}
				onChange={({ detail }) => onChange(detail.selectedOption.value ?? "")}
			/>
		</FormField>
	);
}
export const options = (values: string[]) =>
	values.map((value) => ({ value, label: value }));
