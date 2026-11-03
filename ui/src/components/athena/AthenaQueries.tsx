import { Alert, Box, SpaceBetween, Table } from "@cloudscape-design/components";
import { useCallback, useEffect, useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import {
	Action,
	Buttons,
	Choice,
	Field,
	Rows,
} from "#/components/containers/Controls";
import {
	type AthenaScope,
	athenaCall,
	athenaPages,
	namedQueries,
	queryHistory,
} from "#/lib/athena";
import {
	type NamedQuery,
	namedQueryInput,
	type QueryExecution,
	queryInput,
	type Results,
	rerunOutput,
	resultRows,
	terminal,
} from "#/lib/athenaHelpers";
import { unixTime } from "#/lib/format";
import { usePoll } from "#/lib/usePoll";
export function AthenaQueries({
	scope,
	enabled,
	group,
	draft,
	mode,
	onOpen,
	onChanged,
}: {
	scope: AthenaScope;
	enabled: boolean;
	group: string;
	draft?: QueryExecution | NamedQuery;
	mode: string;
	onOpen: (q: QueryExecution | NamedQuery) => void;
	onChanged: () => void;
}) {
	const executionDraft =
		draft && "QueryExecutionId" in draft ? draft : undefined;
	const [sql, setSQL] = useState(
		executionDraft?.Query ??
			(draft && "QueryString" in draft ? draft.QueryString : "SELECT 1"),
	);
	const [catalog, setCatalog] = useState(
			executionDraft?.QueryExecutionContext?.Catalog ?? "AwsDataCatalog",
		),
		[database, setDatabase] = useState(
			executionDraft?.QueryExecutionContext?.Database ??
				(draft && "Database" in draft ? draft.Database : "default"),
		);
	const [output, setOutput] = useState(rerunOutput(executionDraft)),
		[parameters, setParameters] = useState(
			JSON.stringify(executionDraft?.ExecutionParameters ?? []),
		);
	const [id, setID] = useState(""),
		[busy, setBusy] = useState(false),
		[error, setError] = useState(""),
		[dialog, setDialog] = useState(""),
		[name, setName] = useState(""),
		[description, setDescription] = useState(""),
		[target, setTarget] = useState<NamedQuery>();
	const [results, setResults] = useState<Results>(),
		[tokens, setTokens] = useState<(string | undefined)[]>([undefined]),
		[page, setPage] = useState(0),
		[resultBusy, setResultBusy] = useState(false);
	const resultVersion = useRef(0);
	const activeID = useRef(id);
	activeID.current = id;
	const [pollActive, setPollActive] = useState(false);
	const latest = useRef<QueryExecution | undefined>(undefined);
	const catalogs = usePoll(
		() =>
			enabled && mode === "editor"
				? athenaPages<{ CatalogName: string }>(
						scope,
						"ListDataCatalogs",
						"DataCatalogsSummary",
						{ WorkGroup: group },
					)
				: Promise.resolve([]),
		0,
	);
	const databases = usePoll(
		() =>
			enabled && mode === "editor"
				? athenaPages<{ Name: string }>(
						scope,
						"ListDatabases",
						"DatabaseList",
						{ CatalogName: catalog, WorkGroup: group },
					)
				: Promise.resolve([]),
		0,
	);
	useEffect(() => {
		if (catalog) databases.reload();
	}, [catalog, databases.reload]);
	const history = usePoll(() =>
		enabled && mode === "history"
			? queryHistory(scope, group)
			: Promise.resolve([]),
	);
	const saved = usePoll(() =>
		enabled && mode === "saved"
			? namedQueries(scope, group)
			: Promise.resolve([]),
	);
	const execution = usePoll(
		async () => {
			if (!id) return undefined;
			if (
				latest.current?.QueryExecutionId === id &&
				terminal(latest.current.Status?.State)
			)
				return latest.current;
			const out = await athenaCall<{ QueryExecution?: QueryExecution }>(
				scope,
				"GetQueryExecution",
				{ QueryExecutionId: id },
			);
			if (activeID.current !== id) return latest.current;
			latest.current = out.QueryExecution;
			if (terminal(out.QueryExecution?.Status?.State)) setPollActive(false);
			return out.QueryExecution;
		},
		pollActive ? 1000 : 0,
	);
	const current =
		execution.data?.QueryExecutionId === id ? execution.data : undefined;
	const { account, region } = scope;
	const loadResults = useCallback(
		async (token: string | undefined, index: number) => {
			const version = ++resultVersion.current;
			setResultBusy(true);
			setError("");
			try {
				const out = await athenaCall<Results>(
					{ account, region },
					"GetQueryResults",
					{
						QueryExecutionId: id,
						MaxResults: 100,
						...(token ? { NextToken: token } : {}),
					},
				);
				if (version === resultVersion.current) {
					setResults(out);
					setPage(index);
				}
			} catch (e) {
				if (version === resultVersion.current) setError(String(e));
			} finally {
				if (version === resultVersion.current) setResultBusy(false);
			}
		},
		[account, region, id],
	);
	useEffect(() => {
		if (current?.QueryExecutionId && current.Status?.State === "SUCCEEDED") {
			loadResults(undefined, 0);
		}
	}, [current?.QueryExecutionId, current?.Status?.State, loadResults]);
	async function run() {
		setBusy(true);
		setError("");
		resultVersion.current++;
		setResults(undefined);
		setTokens([undefined]);
		setPage(0);
		setID("");
		try {
			const params: unknown = JSON.parse(parameters);
			if (!Array.isArray(params) || params.some((p) => typeof p !== "string"))
				throw new Error(
					"Execution parameters must be a JSON array of strings.",
				);
			const out = await athenaCall<{ QueryExecutionId: string }>(
				scope,
				"StartQueryExecution",
				{
					...queryInput(sql, group, catalog, database, output, params),
					ClientRequestToken: crypto.randomUUID(),
				},
			);
			setPollActive(true);
			setID(out.QueryExecutionId);
			onChanged();
		} catch (e) {
			setError(String(e));
		} finally {
			setBusy(false);
		}
	}
	useEffect(() => {
		if (id) execution.reload();
	}, [id, execution.reload]);
	const refresh = () => {
		history.reload();
		saved.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="m">
			{[
				error,
				catalogs.error,
				databases.error,
				execution.error,
				history.error,
				saved.error,
			]
				.filter(Boolean)
				.map((e) => (
					<Alert key={e} type="error">
						{e}
					</Alert>
				))}
			{mode === "editor" && (
				<SpaceBetween size="m">
					<Choice
						label="Catalog"
						value={catalog}
						options={(catalogs.data ?? []).map((c) => c.CatalogName)}
						onChange={setCatalog}
					/>
					<Choice
						label="Database"
						value={database}
						options={(databases.data ?? []).map((d) => d.Name)}
						onChange={setDatabase}
					/>
					<Field label="SQL" value={sql} onChange={setSQL} multiline />
					<Field
						label="Output location (S3 URI; blank uses workgroup configuration)"
						value={output}
						onChange={setOutput}
					/>
					<Field
						label="Execution parameters (JSON array of SQL literal strings)"
						value={parameters}
						onChange={setParameters}
					/>
					<Buttons>
						<Action
							disabled={!enabled}
							onClick={() => {
								catalogs.reload();
								databases.reload();
							}}
						>
							Refresh catalog
						</Action>
						<Action
							disabled={
								!enabled || busy || (!!id && !terminal(current?.Status?.State))
							}
							onClick={run}
						>
							{busy ? "Running" : "Run"}
						</Action>
						<Action
							disabled={!enabled || !id || terminal(current?.Status?.State)}
							onClick={async () => {
								try {
									await athenaCall(scope, "StopQueryExecution", {
										QueryExecutionId: id,
									});
									execution.reload();
								} catch (e) {
									setError(String(e));
								}
							}}
						>
							Cancel query
						</Action>
						<Action
							disabled={!enabled}
							onClick={() => {
								setName("");
								setDescription("");
								setDialog("create");
							}}
						>
							Save query
						</Action>
					</Buttons>
					{current && (
						<SpaceBetween size="m">
							<Box>
								Status: {current.Status?.State ?? "-"} —{" "}
								{current.Status?.StateChangeReason ?? ""}
							</Box>
							<Box>
								Submitted: {unixTime(current.Status?.SubmissionDateTime)} /
								completed: {unixTime(current.Status?.CompletionDateTime)}
							</Box>
							<Box>
								Engine: {current.Statistics?.EngineExecutionTimeInMillis ?? "-"}{" "}
								ms; total:{" "}
								{current.Statistics?.TotalExecutionTimeInMillis ?? "-"} ms;
								queue: {current.Statistics?.QueryQueueTimeInMillis ?? "-"} ms;
								planning: {current.Statistics?.QueryPlanningTimeInMillis ?? "-"}{" "}
								ms; processing:{" "}
								{current.Statistics?.ServiceProcessingTimeInMillis ?? "-"} ms;
								scanned: {current.Statistics?.DataScannedInBytes ?? "-"} bytes
							</Box>
							<Box>
								Results: {current.ResultConfiguration?.OutputLocation ?? "-"}
							</Box>
						</SpaceBetween>
					)}
					{results && (
						<SpaceBetween size="m">
							<Table
								loading={resultBusy}
								empty="No result rows."
								items={resultRows(results, page === 0, current?.StatementType)}
								columnDefinitions={(
									results.ResultSet?.ResultSetMetadata?.ColumnInfo ?? []
								).map((c, i) => ({
									id: String(i),
									header: `${c.Label ?? c.Name ?? i} (${c.Type ?? ""})`,
									cell: (row: (string | null)[]) =>
										row[i] === null ? "NULL" : (row[i] ?? ""),
								}))}
							/>
							<Box>
								Updated rows: {results.UpdateCount ?? 0}; page {page + 1}
							</Box>
							<Buttons>
								<Action
									disabled={resultBusy || page === 0}
									onClick={() => loadResults(tokens[page - 1], page - 1)}
								>
									Previous results
								</Action>
								<Action
									disabled={resultBusy || !results.NextToken}
									onClick={() => {
										const token = results.NextToken;
										setTokens((t) => [...t.slice(0, page + 1), token]);
										loadResults(token, page + 1);
									}}
								>
									Next results
								</Action>
							</Buttons>
						</SpaceBetween>
					)}
				</SpaceBetween>
			)}
			{mode === "history" && (
				<Rows
					title="Query history"
					items={history.data ?? []}
					loading={history.loading}
					emptyText="Run SQL in the query editor."
					actions={<Action onClick={refresh}>Refresh</Action>}
					columns={[
						{
							id: "id",
							header: "Execution ID",
							cell: (q) => q.QueryExecutionId,
						},
						{ id: "sql", header: "SQL", cell: (q) => q.Query },
						{ id: "status", header: "Status", cell: (q) => q.Status?.State },
						{
							id: "time",
							header: "Submitted",
							cell: (q) => unixTime(q.Status?.SubmissionDateTime),
						},
						{
							id: "actions",
							header: "Actions",
							cell: (q) => (
								<Action disabled={!enabled} onClick={() => onOpen(q)}>
									Re-run in editor
								</Action>
							),
						},
					]}
				/>
			)}
			{mode === "saved" && (
				<Rows
					title="Saved queries"
					items={saved.data ?? []}
					loading={saved.loading}
					emptyText="Save SQL from the query editor."
					actions={
						<Buttons>
							<Action onClick={refresh}>Refresh</Action>
							<Action
								disabled={!enabled}
								onClick={() => {
									setName("");
									setDescription("");
									setSQL("SELECT 1");
									setDialog("create");
								}}
							>
								Create saved query
							</Action>
						</Buttons>
					}
					columns={[
						{ id: "name", header: "Name", cell: (q) => q.Name },
						{ id: "db", header: "Database", cell: (q) => q.Database },
						{
							id: "description",
							header: "Description",
							cell: (q) => q.Description,
						},
						{
							id: "actions",
							header: "Actions",
							cell: (q) => (
								<Buttons>
									<Action onClick={() => onOpen(q)}>Open in editor</Action>
									<Action
										disabled={!enabled}
										onClick={() => {
											setTarget(q);
											setDialog("delete");
										}}
									>
										Delete
									</Action>
								</Buttons>
							),
						},
					]}
				/>
			)}
			{dialog && (
				<ActionDialog
					title={dialog === "create" ? "Save query" : "Delete saved query"}
					submit={dialog === "create" ? "Save query" : "Delete"}
					confirmation={dialog === "delete" ? target?.Name : undefined}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						refresh();
					}}
					plan={async () => {
						const operation =
							dialog === "create" ? "CreateNamedQuery" : "DeleteNamedQuery";
						const input =
							dialog === "create"
								? {
										...namedQueryInput(name, description, sql, database, group),
										ClientRequestToken: crypto.randomUUID(),
									}
								: { NamedQueryId: target?.NamedQueryId };
						return [
							{
								label: operation,
								run: () => athenaCall(scope, operation, input),
							},
						];
					}}
				>
					{dialog === "create" ? (
						<SpaceBetween size="m">
							<Field label="Name" value={name} onChange={setName} />
							{mode === "saved" && (
								<SpaceBetween size="m">
									<Field
										label="Database"
										value={database}
										onChange={setDatabase}
									/>
									<Field label="SQL" value={sql} onChange={setSQL} multiline />
								</SpaceBetween>
							)}
							<Field
								label="Description"
								value={description}
								onChange={setDescription}
							/>
						</SpaceBetween>
					) : (
						<Alert type="warning">Delete {target?.Name} permanently.</Alert>
					)}
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
