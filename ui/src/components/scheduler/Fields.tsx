import { Autosuggest, FormField } from "@cloudscape-design/components";
import type { ArnOption } from "#/lib/schedulerHelpers";

// Suggest offers existing resources and still accepts any ARN typed in.
export function Suggest({
	label,
	value,
	options,
	onChange,
	placeholder,
	description,
	constraint,
}: {
	label: string;
	value: string;
	options: ArnOption[];
	onChange: (value: string) => void;
	placeholder: string;
	description?: string;
	constraint?: string;
}) {
	return (
		<FormField
			label={label}
			description={description}
			constraintText={constraint}
		>
			<Autosuggest
				ariaLabel={label}
				value={value}
				placeholder={placeholder}
				enteredTextLabel={(text) => `Use ARN: ${text}`}
				empty="No matching resources. Type an ARN instead."
				options={options.map((o) => ({
					value: o.value,
					label: o.label,
					description: o.value,
					tags: [o.description],
				}))}
				onChange={({ detail }) => onChange(detail.value)}
			/>
		</FormField>
	);
}
