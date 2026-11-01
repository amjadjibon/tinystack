import {
	Checkbox,
	FormField,
	Input,
	Select,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { type RDSScope, rdsCall } from "#/lib/rds";
export type RDSField = {
	key: string;
	label: string;
	value?: string | boolean;
	type?: "number" | "password" | "boolean";
	options?: string[];
	required?: boolean;
};
export function RDSOperationDialog({
	scope,
	operation,
	title,
	fields = [],
	input = {},
	confirmation,
	validate,
	onClose,
	onSaved,
}: {
	scope: RDSScope;
	operation: string;
	title: string;
	fields?: RDSField[];
	input?: Record<string, unknown>;
	confirmation?: string;
	validate?: (input: Record<string, unknown>) => void;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [values, setValues] = useState<Record<string, string | boolean>>(
		Object.fromEntries(
			fields.map((f) => [
				f.key,
				f.value ?? (f.type === "boolean" ? false : ""),
			]),
		),
	);
	return (
		<ActionDialog
			title={title}
			submit={title}
			confirmation={confirmation}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const next = { ...input };
				for (const field of fields) {
					const value = values[field.key];
					if (field.required && !value)
						throw new Error(`${field.label} is required.`);
					if (value === "") continue;
					if (field.type === "number") {
						const number = Number(value);
						if (!Number.isFinite(number))
							throw new Error(`${field.label} must be a number.`);
						next[field.key] = number;
					} else next[field.key] = value;
				}
				validate?.(next);
				return [{ label: title, run: () => rdsCall(scope, operation, next) }];
			}}
		>
			<SpaceBetween size="m">
				{fields.map((field) => (
					<FormField key={field.key} label={field.label}>
						{field.type === "boolean" ? (
							<Checkbox
								checked={values[field.key] === true}
								onChange={({ detail }) =>
									setValues({ ...values, [field.key]: detail.checked })
								}
							>
								{field.label}
							</Checkbox>
						) : field.options ? (
							<Select
								ariaLabel={field.label}
								placeholder={`Choose ${field.label.toLowerCase()}`}
								selectedOption={
									values[field.key]
										? { value: String(values[field.key]) }
										: null
								}
								options={field.options.map((value) => ({
									value,
									label: value,
								}))}
								onChange={({ detail }) =>
									setValues({
										...values,
										[field.key]: detail.selectedOption.value ?? "",
									})
								}
							/>
						) : (
							<Input
								ariaLabel={field.label}
								value={String(values[field.key])}
								type={field.type === "password" ? "password" : "text"}
								onChange={({ detail }) =>
									setValues({ ...values, [field.key]: detail.value })
								}
							/>
						)}
					</FormField>
				))}
			</SpaceBetween>
		</ActionDialog>
	);
}
