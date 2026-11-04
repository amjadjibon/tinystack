import {
	Alert,
	Button,
	Container,
	FormField,
	Header,
	Input,
	SpaceBetween,
	Table,
	Textarea,
} from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { errorText } from "#/lib/consoleActions";
import {
	type DynamoScope,
	dynamoCall,
	type Item,
	typedItem,
} from "#/lib/dynamodb";
import { statementInput } from "#/lib/dynamodbHelpers";

export function DynamoPartiQL({
	scope,
	enabled,
	onChanged,
}: {
	scope: DynamoScope;
	enabled: boolean;
	onChanged: (message: string) => void;
}) {
	const [statement, setStatement] = useState(`SELECT * FROM "${scope.table}"`);
	const [parameters, setParameters] = useState("[]"),
		[limit, setLimit] = useState("25");
	const [page, setPage] = useState<{ Items?: Item[]; NextToken?: string }>();
	const [executed, setExecuted] = useState<Record<string, unknown>>();
	const [dialog, setDialog] = useState(false),
		[error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const lock = useRef(false);
	const input = () => {
		const built = statementInput(statement, parameters, limit);
		for (const value of built.Parameters)
			typedItem(JSON.stringify({ parameter: value }), "Parameter");
		return built;
	};
	const run = async (request: Record<string, unknown>) => {
		setPage(await dynamoCall(scope, "ExecuteStatement", request));
		setExecuted(request);
	};
	const rows = page?.Items ?? [];
	const attributes = [...new Set(rows.flatMap((r) => Object.keys(r)))];
	return (
		<SpaceBetween size="l">
			<Container header={<Header variant="h2">PartiQL editor</Header>}>
				<SpaceBetween size="m">
					<Alert>
						Execute SELECT, INSERT, UPDATE or DELETE statements. Statements can
						modify data in the selected account and Region.
					</Alert>
					<FormField label="Statement">
						<Textarea
							disabled={busy}
							value={statement}
							onChange={({ detail }) => setStatement(detail.value)}
							rows={5}
						/>
					</FormField>
					<FormField label="Parameters (DynamoDB JSON array)">
						<Textarea
							disabled={busy}
							value={parameters}
							onChange={({ detail }) => setParameters(detail.value)}
						/>
					</FormField>
					<FormField label="Items evaluated per page">
						<Input
							disabled={busy}
							value={limit}
							onChange={({ detail }) => setLimit(detail.value)}
						/>
					</FormField>
					<Button disabled={!enabled || busy} onClick={() => setDialog(true)}>
						Execute statement
					</Button>
				</SpaceBetween>
			</Container>
			{!!error && <Alert type="error">{error}</Alert>}
			<Table
				items={rows}
				loading={busy}
				header={<Header variant="h2">Statement results</Header>}
				empty={
					executed
						? "No items returned."
						: "Execute a statement to see results."
				}
				resizableColumns
				wrapLines
				columnDefinitions={attributes.map((name) => ({
					id: name,
					header: name,
					cell: (row: Item) =>
						row[name]?.S ??
						row[name]?.N ??
						row[name]?.B ??
						JSON.stringify(row[name]),
				}))}
				footer={
					<Button
						disabled={!enabled || busy || !page?.NextToken}
						loading={busy}
						onClick={async () => {
							if (lock.current || !executed || !page?.NextToken) return;
							lock.current = true;
							setBusy(true);
							setError("");
							try {
								await run({ ...executed, NextToken: page.NextToken });
							} catch (e) {
								setError(errorText(e));
							} finally {
								lock.current = false;
								setBusy(false);
							}
						}}
					>
						Next page
					</Button>
				}
			/>
			{dialog && (
				<ActionDialog
					title="Execute PartiQL statement"
					submit="Execute"
					onClose={() => setDialog(false)}
					onSaved={() => {
						setDialog(false);
						setError("");
						onChanged("Statement executed.");
					}}
					plan={async () => {
						const request = input();
						return [{ label: "Execute statement", run: () => run(request) }];
					}}
				>
					<pre style={{ whiteSpace: "pre-wrap" }}>{statement}</pre>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
