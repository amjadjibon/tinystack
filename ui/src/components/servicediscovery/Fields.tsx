import { Button, SpaceBetween } from "@cloudscape-design/components";
import { Buttons, Field } from "#/components/containers/Controls";
import type { Pair } from "#/lib/servicediscoveryHelpers";
export function Pairs({
	label,
	rows,
	onChange,
}: {
	label: string;
	rows: Pair[];
	onChange: (rows: Pair[]) => void;
}) {
	return (
		<SpaceBetween size="xs">
			<strong>{label}</strong>
			{rows.map((row, index) => (
				// biome-ignore lint/suspicious/noArrayIndexKey: Rows contain controlled inputs with no local state.
				<Buttons key={index}>
					<Field
						label={`${label} key ${index + 1}`}
						value={row.key}
						onChange={(key) =>
							onChange(rows.map((r, i) => (i === index ? { ...r, key } : r)))
						}
					/>
					<Field
						label={`${label} value ${index + 1}`}
						value={row.value}
						onChange={(value) =>
							onChange(rows.map((r, i) => (i === index ? { ...r, value } : r)))
						}
					/>
					<Button onClick={() => onChange(rows.filter((_, i) => i !== index))}>
						Remove
					</Button>
				</Buttons>
			))}
			<Button onClick={() => onChange([...rows, { key: "", value: "" }])}>
				Add {label.toLowerCase()}
			</Button>
		</SpaceBetween>
	);
}
