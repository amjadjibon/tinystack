import { FormField, Textarea } from "@cloudscape-design/components";

export function TagsField({
	value,
	onChange,
}: {
	value: string;
	onChange: (value: string) => void;
}) {
	return (
		<FormField
			label="Tags (JSON)"
			description="Object of string values, for example {}."
		>
			<Textarea
				ariaLabel="Tags"
				value={value}
				onChange={({ detail }) => onChange(detail.value)}
			/>
		</FormField>
	);
}
