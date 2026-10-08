import {
	Alert,
	Box,
	Button,
	Input,
	Modal,
	SpaceBetween,
} from "@cloudscape-design/components";
import { type ReactNode, useRef, useState } from "react";
import { type S3Step, s3Sequence } from "#/lib/s3";

export function S3Dialog({
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
	plan: () => Promise<S3Step[]>;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const [confirm, setConfirm] = useState("");
	const steps = useRef<S3Step[] | null>(null);
	const completed = useRef(new Set<number>());
	const locked = steps.current !== null;
	const save = async () => {
		setBusy(true);
		setError("");
		try {
			steps.current ??= await plan();
			await s3Sequence(steps.current, completed.current);
			onSaved();
		} catch (err) {
			setError(err instanceof Error ? err.message : String(err));
		} finally {
			setBusy(false);
		}
	};
	return (
		<Modal
			visible
			header={title}
			onDismiss={() => !busy && onClose()}
			closeAriaLabel="Close dialog"
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
							{locked ? "Retry remaining steps" : submit}
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
					disabled={busy || locked}
					style={{ border: 0, margin: 0, padding: 0 }}
				>
					{children}
				</fieldset>
				{confirmation && (
					<SpaceBetween size="xs">
						<Box>
							Type <strong>{confirmation}</strong> to confirm.
						</Box>
						<Input
							ariaLabel="Confirm action"
							value={confirm}
							disabled={busy || locked}
							onChange={({ detail }) => setConfirm(detail.value)}
						/>
					</SpaceBetween>
				)}
				{locked && error && (
					<Alert type="info">
						Completed changes remain in place when you close this dialog.
					</Alert>
				)}
			</SpaceBetween>
		</Modal>
	);
}
