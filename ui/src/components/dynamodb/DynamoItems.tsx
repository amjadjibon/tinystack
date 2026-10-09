import {
	Alert,
	Box,
	Button,
	Checkbox,
	Container,
	FormField,
	Header,
	Input,
	Link,
	RadioGroup,
	Select,
	SpaceBetween,
	Table,
	Textarea,
} from "@cloudscape-design/components";
import { useEffect, useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { errorText, integer, stringMap } from "#/lib/consoleActions";
import {
	type DynamoScope,
	type DynamoTable,
	editItemInput,
	type Item,
	itemKey,
	newItem,
	type ReadPage,
	tableCall,
	typedItem,
} from "#/lib/dynamodb";

export function DynamoItems({
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
	const [mode, setMode] = useState("Scan"),
		[index, setIndex] = useState("");
	const [condition, setCondition] = useState("#pk = :pk"),
		[filter, setFilter] = useState(""),
		[projection, setProjection] = useState("");
	const [names, setNames] = useState("{}"),
		[values, setValues] = useState("{}");
	const [limit, setLimit] = useState("25"),
		[consistent, setConsistent] = useState(false),
		[forward, setForward] = useState(true);
	const [page, setPage] = useState<ReadPage>(),
		[pageIndex, setPageIndex] = useState(0);
	const [busy, setBusy] = useState(false),
		[error, setError] = useState("");
	const request = useRef(0),
		locked = useRef(false);
	const cursors = useRef<(Item | undefined)[]>([]);
	const executed = useRef<
		| {
				operation: string;
				input: Record<string, unknown>;
		  }
		| undefined
	>(undefined);
	const schema = table.KeySchema ?? [];
	const indexes = [
		...(table.GlobalSecondaryIndexes ?? []),
		...(table.LocalSecondaryIndexes ?? []),
	];
	const [dialog, setDialog] = useState<"create" | "edit" | "delete" | null>(
			null,
		),
		[text, setText] = useState("{}");
	const [original, setOriginal] = useState<Item>();
	const [opening, setOpening] = useState(false);
	const [editingError, setEditingError] = useState("");
	const openingLock = useRef(false);
	const resetQuery = (nextMode: string, nextIndex: string) => {
		setMode(nextMode);
		setIndex(nextIndex);
		if (nextMode === "Query") {
			const key = (
				indexes.find((i) => i.IndexName === nextIndex)?.KeySchema ?? schema
			).find((k) => k.KeyType === "HASH")?.AttributeName;
			const type =
				table.AttributeDefinitions?.find((d) => d.AttributeName === key)
					?.AttributeType ?? "S";
			setCondition("#pk = :pk");
			setNames(JSON.stringify({ "#pk": key }, null, 2));
			setValues(
				JSON.stringify(
					{
						":pk": {
							[type]: type === "N" ? "0" : type === "B" ? "a2V5" : "value",
						},
					},
					null,
					2,
				),
			);
		} else {
			setNames("{}");
			setValues("{}");
		}
		if (table.GlobalSecondaryIndexes?.some((i) => i.IndexName === nextIndex))
			setConsistent(false);
	};
	const load = async (
		params: { operation: string; input: Record<string, unknown> },
		cursor: Item | undefined,
		position: number,
	) => {
		if (!enabled || locked.current || openingLock.current) return;
		locked.current = true;
		setBusy(true);
		setError("");
		const id = ++request.current;
		try {
			const result = await tableCall<ReadPage>(scope, params.operation, {
				...params.input,
				...(cursor ? { ExclusiveStartKey: cursor } : {}),
			});
			if (id !== request.current) return;
			setPage(result);
			setPageIndex(position);
			cursors.current[position] = cursor;
		} catch (err) {
			if (id === request.current) setError(errorText(err));
		} finally {
			if (id === request.current) {
				locked.current = false;
				setBusy(false);
			}
		}
	};
	const run = () => {
		if (locked.current || openingLock.current || !enabled) return;
		try {
			const n = stringMap(names, "Expression attribute names"),
				v = typedItem(values, "Expression attribute values");
			if (mode === "Query" && !condition.trim())
				throw new Error("Enter a key condition expression for Query.");
			const params = {
				operation: mode,
				input: {
					Limit: integer(limit, "Page evaluation limit", 1, 1000),
					ConsistentRead: consistent,
					...(index ? { IndexName: index } : {}),
					...(mode === "Query"
						? { KeyConditionExpression: condition, ScanIndexForward: forward }
						: {}),
					...(filter.trim() ? { FilterExpression: filter } : {}),
					...(projection.trim() ? { ProjectionExpression: projection } : {}),
					...(Object.keys(n).length ? { ExpressionAttributeNames: n } : {}),
					...(Object.keys(v).length ? { ExpressionAttributeValues: v } : {}),
				},
			};
			executed.current = params;
			cursors.current = [];
			setPage(undefined);
			setPageIndex(0);
			load(params, undefined, 0);
		} catch (err) {
			setError(errorText(err));
		}
	};
	// Invalidate in-flight reads when switching table or account; items are never polled.
	useEffect(
		() => () => {
			request.current++;
			locked.current = false;
			openingLock.current = false;
		},
		[],
	);
	const open = async (kind: "edit" | "delete", row: Item) => {
		if (!enabled || openingLock.current || locked.current) return;
		openingLock.current = true;
		setOpening(true);
		setEditingError("");
		const id = request.current;
		try {
			const result = await tableCall<{ Item?: Item }>(scope, "GetItem", {
				Key: itemKey(row, schema),
				ConsistentRead: true,
			});
			if (id !== request.current) return;
			if (!result.Item || !Object.keys(result.Item).length)
				throw new Error(
					"This item no longer exists. Run the query or scan again.",
				);
			setOriginal(result.Item);
			setText(JSON.stringify(result.Item, null, 2));
			setDialog(kind);
		} catch (err) {
			if (id === request.current) setEditingError(errorText(err));
		} finally {
			if (id === request.current) {
				openingLock.current = false;
				setOpening(false);
			}
		}
	};
	const rows = page?.Items ?? [];
	const attributes = [
		...new Set([
			...schema.map((k) => k.AttributeName),
			...rows.flatMap((row) => Object.keys(row)),
		]),
	].slice(0, 20);
	const display = (value?: Item[string]) =>
		value ? (value.S ?? value.N ?? value.B ?? JSON.stringify(value)) : "—";
	return (
		<SpaceBetween size="l">
			<Container header={<Header variant="h2">Query or scan</Header>}>
				<SpaceBetween size="l">
					{!enabled && (
						<Alert type="info">
							Item reads and writes require unsigned console actions. Use signed
							CLI/SDK requests when this server requires signatures.
						</Alert>
					)}
					{error && (
						<Alert type="error" header="Cannot read items">
							{error}
						</Alert>
					)}
					<fieldset
						disabled={!enabled || busy}
						style={{ border: 0, margin: 0, padding: 0 }}
					>
						<SpaceBetween size="l">
							<RadioGroup
								value={mode}
								onChange={({ detail }) => resetQuery(detail.value, index)}
								items={[
									{ value: "Scan", label: "Scan" },
									{ value: "Query", label: "Query" },
								]}
							/>
							<FormField label="Table or index">
								<Select
									selectedOption={{ value: index, label: index || scope.table }}
									options={[
										{ value: "", label: scope.table },
										...indexes.map((i) => ({
											value: i.IndexName,
											label: i.IndexName,
										})),
									]}
									onChange={({ detail }) =>
										resetQuery(mode, detail.selectedOption.value ?? "")
									}
								/>
							</FormField>
							{mode === "Query" && (
								<FormField
									label="Key condition expression"
									description="Partition key equality, with an optional sort key condition. Use aliases for reserved attribute names."
								>
									<Input
										ariaLabel="Key condition expression"
										value={condition}
										onChange={({ detail }) => setCondition(detail.value)}
									/>
								</FormField>
							)}
							<FormField
								label="Filter expression — optional"
								description="Applied after items are evaluated. A page may be empty and still have a next page."
							>
								<Input
									ariaLabel="Item filter expression"
									value={filter}
									onChange={({ detail }) => setFilter(detail.value)}
								/>
							</FormField>
							<FormField
								label="Projection expression — optional"
								description="Comma-separated attributes. Include the table's primary keys to open, edit or delete results."
							>
								<Input
									ariaLabel="Item projection expression"
									value={projection}
									onChange={({ detail }) => setProjection(detail.value)}
								/>
							</FormField>
							<SpaceBetween direction="horizontal" size="s">
								<FormField label="Expression attribute names (JSON)">
									<Textarea
										ariaLabel="Expression attribute names"
										rows={4}
										value={names}
										onChange={({ detail }) => setNames(detail.value)}
									/>
								</FormField>
								<FormField label="Expression attribute values (DynamoDB JSON)">
									<Textarea
										ariaLabel="Expression attribute values"
										rows={4}
										value={values}
										onChange={({ detail }) => setValues(detail.value)}
									/>
								</FormField>
							</SpaceBetween>
							<FormField
								label="Items evaluated per page"
								constraintText="1–1,000. The API also limits each page to 1 MiB before filters."
							>
								<Input
									ariaLabel="Items evaluated per page"
									type="number"
									value={limit}
									onChange={({ detail }) => setLimit(detail.value)}
								/>
							</FormField>
							<Checkbox
								checked={consistent}
								disabled={table.GlobalSecondaryIndexes?.some(
									(i) => i.IndexName === index,
								)}
								onChange={({ detail }) => setConsistent(detail.checked)}
							>
								Consistent read
							</Checkbox>
							{mode === "Query" && (
								<Checkbox
									checked={forward}
									onChange={({ detail }) => setForward(detail.checked)}
								>
									Sort ascending
								</Checkbox>
							)}
						</SpaceBetween>
					</fieldset>
					<Button
						variant="primary"
						disabled={!enabled}
						loading={busy}
						onClick={run}
					>
						Run
					</Button>
				</SpaceBetween>
			</Container>
			{editingError && (
				<Alert type="error" header="Cannot open item">
					{editingError}
				</Alert>
			)}
			<Table
				items={rows}
				loading={busy || opening}
				loadingText="Loading items"
				resizableColumns
				wrapLines
				header={
					<Header
						variant="h2"
						counter={`(${rows.length})`}
						description={
							page
								? `Page ${pageIndex + 1} · ${page.Count ?? rows.length} returned · ${page.ScannedCount ?? 0} evaluated. Results use the settings from the last Run.`
								: "Run a query or scan to load items. Reads are explicit and never polled."
						}
						actions={
							<Button
								variant="primary"
								disabled={!enabled || !schema.length || busy || opening}
								onClick={() => {
									if (locked.current || openingLock.current) return;
									setOriginal(undefined);
									setText(JSON.stringify(newItem(table), null, 2));
									setEditingError("");
									setDialog("create");
								}}
							>
								Create item
							</Button>
						}
					>
						Items
					</Header>
				}
				columnDefinitions={[
					...attributes.map((name, i) => ({
						id: `attribute-${name}`,
						header: name,
						cell: (row: Item) =>
							i === 0 && schema.every((k) => row[k.AttributeName]) ? (
								<Link onFollow={() => open("edit", row)}>
									{display(row[name])}
								</Link>
							) : (
								display(row[name])
							),
					})),
					{
						id: "_actions",
						header: "Actions",
						cell: (row) => (
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									disabled={
										!enabled ||
										opening ||
										!schema.every((k) => row[k.AttributeName])
									}
									onClick={() => open("edit", row)}
								>
									Edit
								</Button>
								<Button
									disabled={
										!enabled ||
										opening ||
										!schema.every((k) => row[k.AttributeName])
									}
									onClick={() => open("delete", row)}
								>
									Delete
								</Button>
							</SpaceBetween>
						),
					},
				]}
				empty={
					<Box textAlign="center" padding="l">
						{page
							? "No items returned on this page. Check for a next page when using filters."
							: "No items loaded. Run a query or scan."}
					</Box>
				}
				footer={
					<SpaceBetween direction="horizontal" size="xs">
						<Button
							disabled={busy || !executed.current || pageIndex === 0}
							onClick={() =>
								executed.current &&
								load(
									executed.current,
									cursors.current[pageIndex - 1],
									pageIndex - 1,
								)
							}
						>
							Previous page
						</Button>
						<Button
							disabled={
								busy ||
								!executed.current ||
								!Object.keys(page?.LastEvaluatedKey ?? {}).length
							}
							onClick={() =>
								executed.current &&
								load(executed.current, page?.LastEvaluatedKey, pageIndex + 1)
							}
						>
							Next page
						</Button>
					</SpaceBetween>
				}
			/>
			{dialog && (
				<ActionDialog
					title={
						dialog === "create"
							? "Create item"
							: dialog === "delete"
								? "Delete item"
								: "Edit item"
					}
					submit={
						dialog === "create"
							? "Create item"
							: dialog === "delete"
								? "Delete item"
								: "Save changes"
					}
					confirmation={dialog === "delete" ? "delete" : undefined}
					onClose={() => setDialog(null)}
					onSaved={() => {
						setDialog(null);
						onChanged(
							dialog === "create"
								? "Item created."
								: dialog === "delete"
									? "Item deleted."
									: "Item updated.",
						);
						if (executed.current) {
							cursors.current = [];
							load(executed.current, undefined, 0);
						}
					}}
					plan={async () => {
						if (dialog === "delete" && original)
							return [
								{
									label: "Delete item",
									run: () =>
										tableCall(scope, "DeleteItem", {
											Key: itemKey(original, schema),
											ConditionExpression: "attribute_exists(#pk)",
											ExpressionAttributeNames: {
												"#pk": schema[0].AttributeName,
											},
										}),
								},
							];
						const item = typedItem(text);
						itemKey(item, schema);
						if (dialog === "create")
							return [
								{
									label: "Create item",
									run: () =>
										tableCall(scope, "PutItem", {
											Item: item,
											ConditionExpression: "attribute_not_exists(#pk)",
											ExpressionAttributeNames: {
												"#pk": schema[0].AttributeName,
											},
										}),
								},
							];
						if (!original) throw new Error("Reopen the item before editing.");
						const input = editItemInput(original, item, schema);
						return [
							{
								label: "Update item",
								run: () => tableCall(scope, "UpdateItem", input),
							},
						];
					}}
				>
					<SpaceBetween size="l">
						{dialog === "delete" ? (
							<Alert type="warning">
								Delete this item from {scope.table}. The deletion cannot be
								undone.
							</Alert>
						) : (
							<Alert type="info">
								Use DynamoDB JSON, for example{" "}
								{`{"pk":{"S":"user#1"},"score":{"N":"42"}}`}. Binary values use
								base64. Number values stay strings to preserve precision.{" "}
								{dialog === "edit"
									? "Primary keys are immutable. Changes to modified attributes are checked against the loaded values; if a conflict occurs, close and reopen the item."
									: "An existing item with the same primary key will not be overwritten."}
							</Alert>
						)}
						<FormField label="Item (DynamoDB JSON)">
							<Textarea
								ariaLabel="Item DynamoDB JSON"
								rows={16}
								readOnly={dialog === "delete"}
								value={text}
								onChange={({ detail }) => setText(detail.value)}
							/>
						</FormField>
					</SpaceBetween>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
