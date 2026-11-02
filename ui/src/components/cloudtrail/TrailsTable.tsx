import { Alert, Button, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Buttons, Rows } from "#/components/containers/Controls";
import { type CloudTrailScope, cloudtrailCall } from "#/lib/cloudtrail";
import { type Trail, type TrailForm, trailForm } from "#/lib/cloudtrailHelpers";
import { errorText } from "#/lib/consoleActions";
import { usePoll } from "#/lib/usePoll";
import { TrailDetails } from "./TrailDetails";
import { TrailDialog } from "./TrailDialog";

export function TrailsTable({
	scope,
	enabled,
	onChanged,
}: {
	scope: CloudTrailScope;
	enabled: boolean;
	onChanged: () => void;
}) {
	const poll = usePoll(() =>
		enabled
			? cloudtrailCall<{ TrailList?: Trail[] }>(scope, "DescribeTrails").then(
					(out) => out.TrailList ?? [],
				)
			: Promise.resolve([]),
	);
	const [dialog, setDialog] = useState<{
		creating: boolean;
		form: TrailForm;
	}>();
	const [viewing, setViewing] = useState<string>();
	const [action, setAction] = useState<{ operation: string; trail: Trail }>();
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const refresh = () => {
		poll.reload();
		onChanged();
	};
	const edit = async (p: Trail) => {
		setBusy(true);
		setError("");
		try {
			const detail = await cloudtrailCall<{ Trail: Trail }>(scope, "GetTrail", {
				Name: p.Name,
			});
			setDialog({ creating: false, form: trailForm(detail.Trail) });
		} catch (err) {
			setError(errorText(err));
		} finally {
			setBusy(false);
		}
	};
	return (
		<SpaceBetween size="m">
			{(error || poll.error) && (
				<Alert type="error">{error || poll.error}</Alert>
			)}
			<Rows
				title="Trails"
				emptyText="Choose Create trail to add one."
				items={poll.data ?? []}
				loading={poll.loading}
				actions={
					<Buttons>
						<Button onClick={refresh}>Refresh</Button>
						<Button
							variant="primary"
							disabled={!enabled}
							onClick={() => setDialog({ creating: true, form: trailForm() })}
						>
							Create trail
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
						id: "bucket",
						header: "S3 bucket",
						cell: (p) => p.S3BucketName || "-",
					},
					{
						id: "prefix",
						header: "S3 prefix",
						cell: (p) => p.S3KeyPrefix || "-",
					},
					{ id: "arn", header: "ARN", cell: (p) => p.TrailARN || "-" },

					{
						id: "actions",
						header: "Actions",
						cell: (p) => (
							<Buttons>
								<Button disabled={!enabled || busy} onClick={() => edit(p)}>
									Edit
								</Button>
								{["StartLogging", "StopLogging", "DeleteTrail"].map(
									(operation) => (
										<Button
											key={operation}
											disabled={!enabled}
											onClick={() => setAction({ operation, trail: p })}
										>
											{operation
												.replace("Logging", " logging")
												.replace("Trail", " trail")}
										</Button>
									),
								)}
							</Buttons>
						),
					},
				]}
			/>
			{viewing && (
				<TrailDetails
					key={viewing}
					scope={scope}
					name={viewing}
					enabled={enabled}
					onClose={() => setViewing(undefined)}
				/>
			)}
			{dialog && (
				<TrailDialog
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
					title={`${action.operation.replace("Logging", " logging").replace("Trail", " trail")} ${action.trail.Name}`}
					submit={action.operation}
					confirmation={
						action.operation === "DeleteTrail" ? action.trail.Name : undefined
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
								cloudtrailCall(scope, action.operation, {
									Name: action.trail.Name,
								}),
						},
					]}
				>
					<Alert type={action.operation === "DeleteTrail" ? "warning" : "info"}>
						{action.operation === "DeleteTrail"
							? "Deleting this trail permanently removes its configuration. Event history is retained."
							: `${action.operation === "StartLogging" ? "Start" : "Stop"} logging for this trail.`}
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
