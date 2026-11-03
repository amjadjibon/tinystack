import {
	Alert,
	Box,
	Button,
	FormField,
	Input,
	Modal,
	Select,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import type { Resource } from "#/lib/api";
import { type IAMOption, IAMSelection } from "./IAMSelection";

export type IAMField = {
	id: string;
	label: string;
	description?: string;
	type?: "json" | "number" | "select" | "multi";
	required?: boolean;
	readOnly?: boolean;
	options?: IAMOption[];
};
export type IAMDialog = {
	title: string;
	confirm?: string;
	description?: string;
	fields?: IAMField[];
	values?: Record<string, string>;
	completed?: Set<string>;
	submit: (values: Record<string, string>) => Promise<void>;
};

export function IAMFields({
	fields,
	values,
	onChange,
	errors = {},
	disabled = false,
}: {
	fields: IAMField[];
	values: Record<string, string>;
	onChange: (values: Record<string, string>) => void;
	errors?: Record<string, string>;
	disabled?: boolean;
}) {
	return (
		<SpaceBetween size="l">
			{fields.map((field) => {
				const value = values[field.id] ?? "";
				const set = (next: string) => onChange({ ...values, [field.id]: next });
				return (
					<FormField
						key={field.id}
						label={field.label}
						description={field.description}
						errorText={errors[field.id]}
					>
						{field.type === "multi" ? (
							<IAMSelection
								disabled={disabled || field.readOnly}
								title={field.label}
								options={field.options ?? []}
								selected={JSON.parse(value || "[]")}
								onChange={(next) => set(JSON.stringify(next))}
							/>
						) : field.type === "json" ? (
							<Textarea
								disabled={disabled || field.readOnly}
								value={value}
								rows={12}
								onChange={({ detail }) => set(detail.value)}
								spellcheck={false}
							/>
						) : field.type === "select" ? (
							<Select
								disabled={disabled || field.readOnly}
								options={field.options ?? []}
								selectedOption={
									field.options?.find((o) => o.value === value) ?? null
								}
								onChange={({ detail }) =>
									set(detail.selectedOption.value ?? "")
								}
								placeholder="Choose an option"
							/>
						) : (
							<Input
								disabled={disabled || field.readOnly}
								value={value}
								type={field.type === "number" ? "number" : "text"}
								onChange={({ detail }) => set(detail.value)}
							/>
						)}
					</FormField>
				);
			})}
		</SpaceBetween>
	);
}

export function validateIAMFields(
	fields: IAMField[],
	values: Record<string, string>,
) {
	const errors: Record<string, string> = {};
	for (const field of fields) {
		const value = values[field.id] ?? "";
		if (
			field.required &&
			(!value.trim() ||
				(field.type === "multi" && !JSON.parse(value || "[]").length))
		)
			errors[field.id] = `${field.label} is required.`;
		else if (field.type === "json") {
			try {
				const doc = JSON.parse(value);
				if (!doc || Array.isArray(doc) || typeof doc !== "object")
					throw new Error();
			} catch {
				errors[field.id] = "Enter a valid JSON policy object.";
			}
		} else if (
			field.type === "number" &&
			(!Number.isInteger(Number(value)) || Number(value) <= 0)
		) {
			errors[field.id] = "Enter a positive whole number.";
		}
	}
	return errors;
}

export function IAMActionDialog({
	dialog,
	account,
	onClose,
	onSaved,
}: {
	dialog: IAMDialog;
	account: string;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [values, setValues] = useState(dialog.values ?? {});
	const [errors, setErrors] = useState<Record<string, string>>({});
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const [locked, setLocked] = useState(false);
	const submit = async () => {
		const invalid = validateIAMFields(dialog.fields ?? [], values);
		setErrors(invalid);
		if (Object.keys(invalid).length) return;
		setBusy(true);
		setError("");
		try {
			await dialog.submit(values);
			onSaved();
			onClose();
		} catch (err) {
			setLocked((dialog.completed?.size ?? 0) > 0);
			setError(err instanceof Error ? err.message : String(err));
		} finally {
			setBusy(false);
		}
	};
	return (
		<Modal
			visible
			onDismiss={() => !busy && onClose()}
			header={dialog.title}
			size={
				dialog.fields?.some((f) => f.type === "json" || f.type === "multi")
					? "large"
					: "medium"
			}
			footer={
				<Box float="right">
					<SpaceBetween direction="horizontal" size="xs">
						<Button variant="link" disabled={busy} onClick={onClose}>
							Cancel
						</Button>
						<Button variant="primary" loading={busy} onClick={submit}>
							{locked
								? "Retry remaining steps"
								: (dialog.confirm ?? "Save changes")}
						</Button>
					</SpaceBetween>
				</Box>
			}
		>
			<SpaceBetween size="l">
				<Box color="text-body-secondary">Account {account} · Global</Box>
				{dialog.description && <Box>{dialog.description}</Box>}
				{!!error && <Alert type="error">{error}</Alert>}
				<IAMFields
					fields={dialog.fields ?? []}
					values={values}
					onChange={setValues}
					errors={errors}
					disabled={busy || locked}
				/>
			</SpaceBetween>
		</Modal>
	);
}

export type IAMSecret = { AccessKeyId: string; SecretAccessKey: string };
export type IAMActions = {
	resource: Resource;
	enabled: boolean;
	run: (
		operation: string,
		input?: Record<string, unknown>,
	) => Promise<Record<string, unknown>>;
	open: (dialog: IAMDialog) => void;
};
