import {
	Alert,
	Button,
	FormField,
	Input,
	Multiselect,
	Select,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useMemo } from "react";
import {
	type Ec2Row,
	type Ec2Scope,
	type Ec2Section,
	ec2List,
	rowId,
	rowName,
} from "#/lib/ec2";
import { usePoll } from "#/lib/usePoll";
export type Option = { label: string; value: string; description?: string };
export function Field({
	label,
	value,
	onChange,
	multiline = false,
	description,
	disabled = false,
}: {
	label: string;
	value: string;
	onChange: (v: string) => void;
	multiline?: boolean;
	description?: string;
	disabled?: boolean;
}) {
	return (
		<FormField label={label} description={description}>
			{multiline ? (
				<Textarea
					ariaLabel={label}
					rows={6}
					value={value}
					disabled={disabled}
					onChange={({ detail }) => onChange(detail.value)}
				/>
			) : (
				<Input
					ariaLabel={label}
					value={value}
					disabled={disabled}
					onChange={({ detail }) => onChange(detail.value)}
				/>
			)}
		</FormField>
	);
}
export function Choice({
	label,
	value,
	onChange,
	options,
	description,
	optional = false,
}: {
	label: string;
	value: string;
	onChange: (v: string) => void;
	options: Option[];
	description?: string;
	optional?: boolean;
}) {
	const all = optional ? [{ label: "None", value: "" }, ...options] : options;
	return (
		<FormField label={label} description={description}>
			<Select
				ariaLabel={label}
				selectedOption={
					all.find((o) => o.value === value) ??
					(value ? { label: value, value } : null)
				}
				placeholder={`Choose ${label.toLowerCase()}`}
				options={all}
				onChange={({ detail }) => onChange(detail.selectedOption.value ?? "")}
			/>
		</FormField>
	);
}
export function Choices({
	label,
	value,
	onChange,
	options,
	description,
}: {
	label: string;
	value: string[];
	onChange: (v: string[]) => void;
	options: Option[];
	description?: string;
}) {
	return (
		<FormField label={label} description={description}>
			<Multiselect
				ariaLabel={label}
				selectedOptions={value.map(
					(v) => options.find((o) => o.value === v) ?? { label: v, value: v },
				)}
				options={options}
				placeholder={`Choose ${label.toLowerCase()}`}
				onChange={({ detail }) =>
					onChange(detail.selectedOptions.map((o) => o.value ?? ""))
				}
			/>
		</FormField>
	);
}
export const optionsFor = (section: Ec2Section, items: Ec2Row[]): Option[] =>
	items.map((r) => ({
		label: rowName(r)
			? `${rowName(r)} (${rowId(section, r)})`
			: rowId(section, r),
		value: rowId(section, r),
	}));
export function useChoices(scope: Ec2Scope, kinds: Ec2Section[]) {
	const key = kinds.join(",");
	// biome-ignore lint/correctness/useExhaustiveDependencies: kinds is represented by its stable key.
	const sections = useMemo(() => kinds, [key]);
	const poll = usePoll(
		async () =>
			Object.fromEntries(
				await Promise.all(
					sections.map(
						async (section) =>
							[section, await ec2List(scope, section)] as const,
					),
				),
			) as Partial<Record<Ec2Section, Ec2Row[]>>,
		0,
	);
	return {
		...poll,
		items: (section: Ec2Section) => poll.data?.[section] ?? [],
	};
}
export function ChoiceStatus({
	error,
	loading,
	reload,
}: {
	error?: string;
	loading: boolean;
	reload: () => void;
}) {
	return (
		<>
			{loading && (
				<Alert type="info">Loading resources in this account and Region…</Alert>
			)}
			{!!error && (
				<Alert type="error" header="Cannot load resource choices">
					<SpaceBetween size="xs">
						{error}
						<Button onClick={reload}>Retry</Button>
					</SpaceBetween>
				</Alert>
			)}
		</>
	);
}
