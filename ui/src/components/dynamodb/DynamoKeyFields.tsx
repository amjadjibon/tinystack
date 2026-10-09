import {
	FormField,
	Input,
	Select,
	SpaceBetween,
} from "@cloudscape-design/components";
export function DynamoKeyFields({
	name,
	type,
	label,
	onChange,
}: {
	name: string;
	type: string;
	label: string;
	onChange: (name: string, type: string) => void;
}) {
	return (
		<SpaceBetween direction="horizontal" size="s">
			<FormField label={label}>
				<Input
					ariaLabel={label}
					value={name}
					onChange={({ detail }) => onChange(detail.value, type)}
				/>
			</FormField>
			<FormField label={`${label} type`}>
				<Select
					selectedOption={{
						value: type,
						label: { S: "String", N: "Number", B: "Binary" }[type],
					}}
					options={[
						{ value: "S", label: "String" },
						{ value: "N", label: "Number" },
						{ value: "B", label: "Binary" },
					]}
					onChange={({ detail }) =>
						onChange(name, detail.selectedOption.value ?? "S")
					}
				/>
			</FormField>
		</SpaceBetween>
	);
}
