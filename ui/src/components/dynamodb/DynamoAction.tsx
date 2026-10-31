import {
	Button,
	FormField,
	Input,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";

// Each instance owns its dialog and freezes its values before execution.
export function DynamoAction({
	label,
	enabled,
	fields = [],
	destructive,
	run,
	onSaved,
}: {
	label: string;
	enabled: boolean;
	fields?: {
		key: string;
		label: string;
		initial?: string;
		validate?: (value: string) => unknown;
	}[];
	destructive?: boolean;
	run: (values: Record<string, string>) => Promise<unknown>;
	onSaved: () => void;
}) {
	const [open, setOpen] = useState(false);
	const [values, setValues] = useState<Record<string, string>>({});
	return (
		<>
			<Button
				disabled={!enabled}
				onClick={() => {
					setValues(
						Object.fromEntries(fields.map((f) => [f.key, f.initial ?? ""])),
					);
					setOpen(true);
				}}
			>
				{label}
			</Button>
			{open && (
				<ActionDialog
					title={label}
					submit={label}
					confirmation={destructive ? "delete" : undefined}
					onClose={() => setOpen(false)}
					onSaved={() => {
						setOpen(false);
						onSaved();
					}}
					plan={async () => {
						const snapshot = { ...values };
						for (const field of fields) field.validate?.(snapshot[field.key]);
						return [{ label, run: () => run(snapshot) }];
					}}
				>
					<SpaceBetween size="m">
						{fields.map((f) => (
							<FormField key={f.key} label={f.label}>
								<Input
									value={values[f.key] ?? ""}
									onChange={({ detail }) =>
										setValues({ ...values, [f.key]: detail.value })
									}
								/>
							</FormField>
						))}
					</SpaceBetween>
				</ActionDialog>
			)}
		</>
	);
}
