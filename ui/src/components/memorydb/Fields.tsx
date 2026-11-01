import {
	FormField,
	Input,
	Multiselect,
	Select,
} from "@cloudscape-design/components";

export type Option = { value: string; label?: string };

export function Text({
	label,
	value,
	onChange,
	description,
	type = "text",
	disabled = false,
	constraint,
}: {
	label: string;
	value: string;
	onChange: (value: string) => void;
	description?: string;
	type?: "text" | "password";
	disabled?: boolean;
	constraint?: string;
}) {
	return (
		<FormField
			label={label}
			description={description}
			constraintText={constraint}
		>
			<Input
				ariaLabel={label}
				type={type}
				value={value}
				disabled={disabled}
				onChange={({ detail }) => onChange(detail.value)}
			/>
		</FormField>
	);
}

export function Pick({
	label,
	value,
	options,
	onChange,
	placeholder,
	description,
	disabled = false,
}: {
	label: string;
	value: string;
	options: Option[];
	onChange: (value: string) => void;
	placeholder: string;
	description?: string;
	disabled?: boolean;
}) {
	const selected = options.find((o) => o.value === value);
	return (
		<FormField label={label} description={description}>
			<Select
				ariaLabel={label}
				disabled={disabled}
				placeholder={placeholder}
				empty="Nothing to choose from yet."
				selectedOption={
					value
						? { value, label: selected?.label ?? selected?.value ?? value }
						: null
				}
				options={options.map((o) => ({
					value: o.value,
					label: o.label ?? o.value,
				}))}
				onChange={({ detail }) => onChange(detail.selectedOption.value ?? "")}
			/>
		</FormField>
	);
}

export function PickMany({
	label,
	values,
	options,
	onChange,
	placeholder,
	description,
}: {
	label: string;
	values: string[];
	options: Option[];
	onChange: (values: string[]) => void;
	placeholder: string;
	description?: string;
}) {
	const all = options.map((o) => ({
		value: o.value,
		label: o.label ?? o.value,
	}));
	return (
		<FormField label={label} description={description}>
			<Multiselect
				ariaLabel={label}
				placeholder={placeholder}
				empty="Nothing to choose from yet."
				keepOpen
				selectedOptions={all.filter((o) => values.includes(o.value))}
				options={all}
				onChange={({ detail }) =>
					onChange(detail.selectedOptions.map((o) => o.value ?? ""))
				}
			/>
		</FormField>
	);
}
