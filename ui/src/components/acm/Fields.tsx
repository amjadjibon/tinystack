import { FormField, Select } from "@cloudscape-design/components";

export function Choice({
	label,
	value,
	options,
	onChange,
	description,
}: {
	label: string;
	value: string;
	options: string[];
	onChange: (v: string) => void;
	description?: string;
}) {
	return (
		<FormField label={label} description={description}>
			<Select
				ariaLabel={label}
				placeholder={`Choose ${label.toLowerCase()}`}
				selectedOption={value ? { label: value, value } : null}
				options={options.map((value) => ({ label: value, value }))}
				onChange={({ detail }) => onChange(detail.selectedOption.value ?? "")}
			/>
		</FormField>
	);
}
