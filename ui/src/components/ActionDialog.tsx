import {
	Alert,
	Box,
	Button,
	FormField,
	Input,
	Modal,
	SpaceBetween,
} from "@cloudscape-design/components";
import { type ReactNode, useRef, useState } from "react";
import { type ActionStep, errorText } from "#/lib/consoleActions";

export function ActionDialog({
	title,
	submit = "Save changes",
	confirmation,
	children,
	plan,
	onClose,
	onSaved,
}: {
	title: string;
	submit?: string;
	confirmation?: string;
	children: ReactNode;
	plan: () => Promise<ActionStep[]>;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const [confirm, setConfirm] = useState("");
	const steps = useRef<ActionStep[] | null>(null);
	const completed = useRef(new Set<number>());
	const save = async () => {
		if (busy) return;
		setBusy(true);
		setError("");
		try {
			steps.current ??= await plan();
			for (const [i, step] of steps.current.entries()) {
				if (completed.current.has(i)) continue;
				try {
					await step.run();
					completed.current.add(i);
				} catch (err) {
					throw new Error(`${step.label}: ${errorText(err)}`);
				}
			}
			onSaved();
		} catch (err) {
			setError(errorText(err));
			if (completed.current.size === 0) steps.current = null;
		} finally {
			setBusy(false);
		}
	};
	return (
		<Modal
			visible
			size="large"
			header={title}
			closeAriaLabel="Close dialog"
			onDismiss={() => !busy && onClose()}
			footer={
				<Box float="right">
					<SpaceBetween direction="horizontal" size="xs">
						<Button disabled={busy} onClick={onClose}>
							Cancel
						</Button>
						<Button
							variant="primary"
							loading={busy}
							disabled={!!confirmation && confirm !== confirmation}
							onClick={save}
						>
							{steps.current ? "Retry remaining steps" : submit}
						</Button>
					</SpaceBetween>
				</Box>
			}
		>
			<SpaceBetween size="l">
				{error && (
					<Alert type="error" header="Action failed">
						{error}
					</Alert>
				)}
				<fieldset
					disabled={busy || steps.current !== null}
					style={{ border: 0, margin: 0, padding: 0 }}
				>
					{children}
				</fieldset>
				{confirmation && (
					<FormField
						label={
							<>
								Type <strong>{confirmation}</strong> to confirm.
							</>
						}
					>
						<Input
							ariaLabel="Confirm action"
							value={confirm}
							disabled={busy}
							onChange={({ detail }) => setConfirm(detail.value)}
						/>
					</FormField>
				)}
				{steps.current && error && (
					<Alert type="info">
						Completed changes remain in place. Retrying resumes unfinished
						steps.
					</Alert>
				)}
			</SpaceBetween>
		</Modal>
	);
}
