import { Alert, Button, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Buttons, Rows } from "#/components/containers/Controls";
import { unixTime } from "#/lib/format";
import { type GlueScope, glueCall, listGlue } from "#/lib/glue";
import {
	emptyGlueDefinition,
	type GlueContext,
	type GlueKind,
	type GlueRecord,
	glueDefinition,
	glueIdentity,
	glueName,
} from "#/lib/glueHelpers";
import { usePoll } from "#/lib/usePoll";
import { GlueDetails } from "./GlueDetails";
import { GlueDialog } from "./GlueDialog";
export function GlueResources({
	scope,
	kind,
	context,
	enabled,
	onChanged,
	onSelect,
}: {
	scope: GlueScope;
	kind: GlueKind;
	context: GlueContext;
	enabled: boolean;
	onChanged: () => void;
	onSelect?: (row: GlueRecord) => void;
}) {
	const poll = usePoll(() =>
		enabled ? listGlue(scope, kind, context) : Promise.resolve([]),
	);
	const [dialog, setDialog] = useState<{
		creating: boolean;
		initial: GlueRecord;
	}>();
	const [view, setView] = useState<GlueRecord>();
	const [action, setAction] = useState<{
		operation: string;
		row: GlueRecord;
	}>();
	const refresh = () => {
		poll.reload();
		onChanged();
	};
	const extra =
		kind === "Crawler"
			? ["StartCrawler", "StopCrawler"]
			: kind === "Job"
				? ["StartJobRun"]
				: kind === "Trigger"
					? ["StartTrigger", "StopTrigger"]
					: [];
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows
				title={`${kind === "Partition" ? "Partitions" : `${kind}s`}`}
				items={poll.data ?? []}
				loading={poll.loading}
				emptyText={`Choose Create ${kind.toLowerCase()} to add one.`}
				actions={
					<Buttons>
						<Button onClick={refresh}>Refresh</Button>
						<Button
							variant="primary"
							disabled={!enabled}
							onClick={() =>
								setDialog({
									creating: true,
									initial: emptyGlueDefinition(kind),
								})
							}
						>
							Create {kind.toLowerCase()}
						</Button>
					</Buttons>
				}
				columns={[
					{
						id: "name",
						header: kind === "Partition" ? "Values" : "Name",
						cell: (r) => (
							<Button variant="link" onClick={() => setView(r)}>
								{glueName(r)}
							</Button>
						),
					},
					{
						id: "description",
						header: "Description",
						cell: (r) => String(r.Description ?? "-"),
					},
					{
						id: "state",
						header: "State / type",
						cell: (r) => String(r.State ?? r.TableType ?? "-"),
					},
					{
						id: "created",
						header: "Created",
						cell: (r) =>
							unixTime(
								(r.CreateTime ?? r.CreatedOn ?? r.CreationTime) as number,
							),
					},
					...(kind === "Crawler"
						? [
								{
									id: "crawl",
									header: "Last crawl",
									cell: (r: GlueRecord) => {
										const last = r.LastCrawl as
											| { Status?: string; StartTime?: number }
											| undefined;
										return `${last?.Status ?? "-"} · ${unixTime(last?.StartTime)}`;
									},
								},
							]
						: []),
					{
						id: "actions",
						header: "Actions",
						cell: (r) => (
							<Buttons>
								{onSelect && (
									<Button onClick={() => onSelect(r)}>
										{kind === "Database" ? "Tables" : "Partitions"}
									</Button>
								)}
								{kind !== "Partition" && (
									<Button
										disabled={!enabled}
										onClick={() =>
											setDialog({
												creating: false,
												initial: glueDefinition(kind, r),
											})
										}
									>
										Edit
									</Button>
								)}
								{[...extra, `Delete${kind}`].map((operation) => (
									<Button
										key={operation}
										disabled={!enabled}
										onClick={() => setAction({ operation, row: r })}
									>
										{operation.startsWith("Delete")
											? "Delete"
											: operation === "StartJobRun"
												? "Start run"
												: operation.startsWith("Start")
													? "Start"
													: "Stop"}
									</Button>
								))}
							</Buttons>
						),
					},
				]}
			/>
			{view && (
				<GlueDetails
					scope={scope}
					kind={kind}
					row={view}
					context={context}
					enabled={enabled}
					onClose={() => setView(undefined)}
				/>
			)}
			{dialog && (
				<GlueDialog
					scope={scope}
					kind={kind}
					context={context}
					{...dialog}
					onClose={() => setDialog(undefined)}
					onSaved={() => {
						setDialog(undefined);
						refresh();
					}}
				/>
			)}
			{action && (
				<ActionDialog
					title={`${action.operation}: ${glueName(action.row)}`}
					submit={action.operation.startsWith("Delete") ? "Delete" : "Confirm"}
					confirmation={
						action.operation.startsWith("Delete")
							? glueName(action.row)
							: undefined
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
								glueCall(
									scope,
									action.operation,
									glueIdentity(kind, action.row, context),
								),
						},
					]}
				>
					<Alert
						type={action.operation.startsWith("Delete") ? "warning" : "info"}
					>
						{action.operation.startsWith("Delete")
							? `Permanently delete this ${kind.toLowerCase()}${kind === "Database" ? " and all its tables and partitions" : kind === "Table" ? " and all its partitions" : ""}.`
							: `Perform ${action.operation} for ${glueName(action.row)}.`}
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
