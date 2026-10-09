import {
	Box,
	FormField,
	Input,
	Select,
	SpaceBetween,
} from "@cloudscape-design/components";
import type { IndexInput } from "#/lib/dynamodb";
import { DynamoKeyFields } from "./DynamoKeyFields";
export function DynamoIndexFields({
	value,
	onChange,
	provisioned,
	local = false,
}: {
	value: IndexInput;
	onChange: (value: IndexInput) => void;
	provisioned: boolean;
	local?: boolean;
}) {
	const set = (change: Partial<IndexInput>) =>
		onChange({ ...value, ...change });
	return (
		<SpaceBetween size="l">
			<FormField
				label="Index name"
				constraintText="3–255 letters, numbers, underscores, hyphens or periods."
			>
				<Input
					ariaLabel="Index name"
					value={value.name}
					onChange={({ detail }) => set({ name: detail.value })}
				/>
			</FormField>
			{local ? (
				<Box>Partition key: {value.partition} (same as the table)</Box>
			) : (
				<DynamoKeyFields
					label="Index partition key"
					name={value.partition}
					type={value.partitionType}
					onChange={(partition, partitionType) =>
						set({ partition, partitionType })
					}
				/>
			)}
			<DynamoKeyFields
				label={local ? "Index sort key" : "Index sort key — optional"}
				name={value.sort}
				type={value.sortType}
				onChange={(sort, sortType) => set({ sort, sortType })}
			/>
			<FormField label="Attribute projection">
				<Select
					selectedOption={{ value: value.projection, label: value.projection }}
					options={["ALL", "KEYS_ONLY", "INCLUDE"].map((v) => ({
						value: v,
						label: v,
					}))}
					onChange={({ detail }) =>
						set({ projection: detail.selectedOption.value ?? "ALL" })
					}
				/>
			</FormField>
			{value.projection === "INCLUDE" && (
				<FormField
					label="Non-key attributes"
					description="Comma-separated names to include in index results."
				>
					<Input
						ariaLabel="Projected attribute names"
						value={value.nonKeys}
						onChange={({ detail }) => set({ nonKeys: detail.value })}
					/>
				</FormField>
			)}
			{provisioned && !local && (
				<SpaceBetween direction="horizontal" size="s">
					<FormField label="Index read capacity units">
						<Input
							ariaLabel="Index read capacity units"
							type="number"
							value={value.read}
							onChange={({ detail }) => set({ read: detail.value })}
						/>
					</FormField>
					<FormField label="Index write capacity units">
						<Input
							ariaLabel="Index write capacity units"
							type="number"
							value={value.write}
							onChange={({ detail }) => set({ write: detail.value })}
						/>
					</FormField>
				</SpaceBetween>
			)}
		</SpaceBetween>
	);
}
