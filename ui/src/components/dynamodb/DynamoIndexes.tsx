import {
	Alert,
	Box,
	Button,
	Header,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import {
	attributeDefinitions,
	buildIndex,
	type DynamoIndex,
	type DynamoScope,
	type DynamoTable,
	indexDefaults,
	indexDefinitions,
	tableCall,
} from "#/lib/dynamodb";
import { DynamoIndexFields } from "./DynamoIndexFields";
export function DynamoIndexes({
	scope,
	table,
	enabled,
	onChanged,
}: {
	scope: DynamoScope;
	table: DynamoTable;
	enabled: boolean;
	onChanged: (message: string) => void;
}) {
	const indexes = [
		...(table.GlobalSecondaryIndexes ?? []).map((i) => ({
			...i,
			kind: "Global",
		})),
		...(table.LocalSecondaryIndexes ?? []).map((i) => ({
			...i,
			kind: "Local",
		})),
	];
	const [dialog, setDialog] = useState<"create" | "delete" | null>(null);
	const [selected, setSelected] = useState<DynamoIndex>();
	const [input, setInput] = useState(indexDefaults);
	const provisioned =
		table.BillingModeSummary?.BillingMode !== "PAY_PER_REQUEST";
	return (
		<SpaceBetween size="l">
			<Alert type="info">
				Indexes are derived from table items and become active immediately in
				TinyStack. Local indexes can only be created with the table.
			</Alert>
			<Table
				items={indexes}
				trackBy="IndexName"
				wrapLines
				header={
					<Header
						variant="h2"
						counter={`(${indexes.length})`}
						actions={
							<Button
								variant="primary"
								disabled={
									!enabled ||
									!table.TableName ||
									(table.GlobalSecondaryIndexes?.length ?? 0) >= 20
								}
								onClick={() => {
									setInput(indexDefaults());
									setDialog("create");
								}}
							>
								Create global index
							</Button>
						}
					>
						Secondary indexes
					</Header>
				}
				columnDefinitions={[
					{ id: "name", header: "Index name", cell: (i) => i.IndexName },
					{ id: "type", header: "Type", cell: (i) => i.kind },
					{
						id: "keys",
						header: "Key schema",
						cell: (i) =>
							i.KeySchema.map((k) => `${k.AttributeName} (${k.KeyType})`).join(
								", ",
							),
					},
					{
						id: "projection",
						header: "Projection",
						cell: (i) =>
							`${i.Projection?.ProjectionType ?? "ALL"}${i.Projection?.NonKeyAttributes?.length ? `: ${i.Projection.NonKeyAttributes.join(", ")}` : ""}`,
					},
					{
						id: "state",
						header: "Status",
						cell: (i) => i.IndexStatus ?? "ACTIVE",
					},
					{ id: "items", header: "Items", cell: (i) => i.ItemCount ?? 0 },
					{
						id: "actions",
						header: "Actions",
						cell: (i) =>
							i.kind === "Global" ? (
								<Button
									disabled={!enabled}
									onClick={() => {
										setSelected(i);
										setDialog("delete");
									}}
								>
									Delete
								</Button>
							) : (
								"—"
							),
					},
				]}
				empty={
					<Box textAlign="center" padding="l">
						No secondary indexes.
					</Box>
				}
			/>
			{dialog && (
				<ActionDialog
					title={
						dialog === "create"
							? "Create global secondary index"
							: "Delete global secondary index"
					}
					submit={dialog === "create" ? "Create index" : "Delete index"}
					confirmation={dialog === "delete" ? selected?.IndexName : undefined}
					onClose={() => setDialog(null)}
					onSaved={() => {
						setDialog(null);
						onChanged(
							dialog === "create"
								? "Global index created."
								: "Global index deleted.",
						);
					}}
					plan={async () => {
						if (dialog === "delete")
							return [
								{
									label: "Delete global index",
									run: () =>
										tableCall(scope, "UpdateTable", {
											GlobalSecondaryIndexUpdates: [
												{ Delete: { IndexName: selected?.IndexName } },
											],
										}),
								},
							];
						const index = buildIndex(input, provisioned);
						if (indexes.some((i) => i.IndexName === index.IndexName))
							throw new Error("An index with this name already exists.");
						const definitions = attributeDefinitions([
							...(table.AttributeDefinitions ?? []),
							...indexDefinitions(input),
						]);
						return [
							{
								label: "Create global index",
								run: () =>
									tableCall(scope, "UpdateTable", {
										AttributeDefinitions: definitions,
										GlobalSecondaryIndexUpdates: [{ Create: index }],
									}),
							},
						];
					}}
				>
					{dialog === "create" ? (
						<DynamoIndexFields
							value={input}
							onChange={setInput}
							provisioned={provisioned}
						/>
					) : (
						<Alert type="warning">
							Delete {selected?.IndexName}. This removes the index; items remain
							in the table.
						</Alert>
					)}
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
