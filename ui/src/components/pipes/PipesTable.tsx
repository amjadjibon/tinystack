import { Alert, Button, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Buttons, Rows } from "#/components/containers/Controls";
import { errorText } from "#/lib/consoleActions";
import { unixTime } from "#/lib/format";
import { listPipes, type PipesScope, pipesCall } from "#/lib/pipes";
import {
	emptyPipeForm,
	formFromPipe,
	type Pipe,
	type PipeForm,
} from "#/lib/pipesHelpers";
import { usePoll } from "#/lib/usePoll";
import { PipeDetails } from "./PipeDetails";
import { PipeDialog } from "./PipeDialog";

export function PipesTable({
	scope,
	enabled,
	onChanged,
}: {
	scope: PipesScope;
	enabled: boolean;
	onChanged: () => void;
}) {
	const poll = usePoll(() =>
		enabled ? listPipes(scope) : Promise.resolve([]),
	);
	const [dialog, setDialog] = useState<{ creating: boolean; form: PipeForm }>();
	const [viewing, setViewing] = useState<string>();
	const [action, setAction] = useState<{ operation: string; pipe: Pipe }>();
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const refresh = () => {
		poll.reload();
		onChanged();
	};
	const edit = async (p: Pipe) => {
		setBusy(true);
		setError("");
		try {
			const detail = await pipesCall<Pipe>(scope, "DescribePipe", {
				Name: p.Name,
			});
			setDialog({ creating: false, form: formFromPipe(detail) });
		} catch (err) {
			setError(errorText(err));
		} finally {
			setBusy(false);
		}
	};
	return (
		<SpaceBetween size="m">
			{!!(error || poll.error) && (
				<Alert type="error">{error || poll.error}</Alert>
			)}
			<Rows
				title="Pipes"
				emptyText="Choose Create pipe to add one."
				items={poll.data ?? []}
				loading={poll.loading}
				actions={
					<Buttons>
						<Button onClick={refresh}>Refresh</Button>
						<Button
							variant="primary"
							disabled={!enabled}
							onClick={() =>
								setDialog({ creating: true, form: emptyPipeForm() })
							}
						>
							Create pipe
						</Button>
					</Buttons>
				}
				columns={[
					{
						id: "name",
						header: "Name",
						cell: (p) => (
							<Button variant="link" onClick={() => setViewing(p.Name)}>
								{p.Name}
							</Button>
						),
					},
					{
						id: "current",
						header: "Current state",
						cell: (p) => p.CurrentState || "-",
					},
					{
						id: "desired",
						header: "Desired state",
						cell: (p) => p.DesiredState || "-",
					},
					{ id: "source", header: "Source", cell: (p) => p.Source || "-" },
					{ id: "target", header: "Target", cell: (p) => p.Target || "-" },
					{
						id: "created",
						header: "Created",
						cell: (p) => unixTime(p.CreationTime),
					},
					{
						id: "modified",
						header: "Last modified",
						cell: (p) => unixTime(p.LastModifiedTime),
					},
					{
						id: "actions",
						header: "Actions",
						cell: (p) => (
							<Buttons>
								<Button disabled={!enabled || busy} onClick={() => edit(p)}>
									Edit
								</Button>
								{["StartPipe", "StopPipe", "DeletePipe"].map((operation) => (
									<Button
										key={operation}
										disabled={!enabled}
										onClick={() => setAction({ operation, pipe: p })}
									>
										{operation.replace("Pipe", "")}
									</Button>
								))}
							</Buttons>
						),
					},
				]}
			/>
			{!!viewing && (
				<PipeDetails
					key={viewing}
					scope={scope}
					name={viewing}
					enabled={enabled}
					onClose={() => setViewing(undefined)}
				/>
			)}
			{dialog && (
				<PipeDialog
					scope={scope}
					{...dialog}
					initial={dialog.form}
					onClose={() => setDialog(undefined)}
					onSaved={() => {
						setDialog(undefined);
						refresh();
					}}
				/>
			)}
			{action && (
				<ActionDialog
					title={`${action.operation.replace("Pipe", "")} ${action.pipe.Name}`}
					submit={action.operation.replace("Pipe", " pipe")}
					confirmation={
						action.operation === "DeletePipe" ? action.pipe.Name : undefined
					}
					onClose={() => setAction(undefined)}
					onSaved={() => {
						setAction(undefined);
						refresh();
					}}
					plan={async () => [
						{
							label: action.operation,
							run: () =>
								pipesCall(scope, action.operation, { Name: action.pipe.Name }),
						},
					]}
				>
					<Alert type={action.operation === "DeletePipe" ? "warning" : "info"}>
						{action.operation === "DeletePipe"
							? "Deleting this pipe stops delivery and permanently removes its configuration."
							: `${action.operation === "StartPipe" ? "Start" : "Stop"} event delivery for this pipe.`}
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
