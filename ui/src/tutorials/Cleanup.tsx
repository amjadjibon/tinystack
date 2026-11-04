import {
	Alert,
	Box,
	Button,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { api } from "#/lib/api";
import type { ActionStep } from "#/lib/consoleActions";
import type { Context } from "./checks";
import type { Call, Tutorial } from "./types";
export function Cleanup({
	tutorial,
	ctx,
}: {
	tutorial: Tutorial;
	ctx: Context;
}) {
	const [rows, setRows] = useState<ActionStep[] | null>(null);
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const [done, setDone] = useState(false);
	const call: Call = (service, operation, input) =>
		api.action(service, operation, ctx.account, input, ctx.region);
	const prepare = async () => {
		setBusy(true);
		setError("");
		try {
			setRows(await tutorial.cleanup(ctx, call));
		} catch (e) {
			setError(e instanceof Error ? e.message : String(e));
		} finally {
			setBusy(false);
		}
	};
	return (
		<SpaceBetween size="s">
			{!!error && <Alert type="error">{error}</Alert>}
			<Button disabled={done} loading={busy} onClick={prepare}>
				{done ? "Cleaned up" : "Clean up"}
			</Button>
			{rows && (
				<ActionDialog
					title="Clean up tutorial resources"
					submit="Delete listed resources"
					confirmation="delete"
					onClose={() => setRows(null)}
					onSaved={() => {
						setRows(null);
						setDone(true);
					}}
					plan={async () => rows}
				>
					<Box>
						Only these generated resources in account {ctx.account}, Region{" "}
						{ctx.region} will be removed:
					</Box>
					<ul>
						{rows.map(({ label }) => (
							<li key={label}>{label}</li>
						))}
					</ul>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
