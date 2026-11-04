import {
	Alert,
	Box,
	Button,
	Container,
	FormField,
	Header,
	Input,
	KeyValuePairs,
	Select,
	SpaceBetween,
	Table,
	Textarea,
} from "@cloudscape-design/components";
import { useEffect, useRef, useState } from "react";
import type { Resource } from "#/lib/api";
import { errorText, integer } from "#/lib/consoleActions";
import { type LogsScope, logsCall, logTime } from "#/lib/logs";

type QueryResult = {
	Status?: string;
	Results?: { Field: string; Value: string }[][];
	Statistics?: {
		RecordsMatched?: number;
		RecordsScanned?: number;
		BytesScanned?: number;
	};
};
export function LogsInsights({
	scope,
	groups,
	enabled,
}: {
	scope: LogsScope;
	groups: Resource[];
	enabled: boolean;
}) {
	const [group, setGroup] = useState(scope.group ?? groups[0]?.name ?? ""),
		[start, setStart] = useState(new Date(Date.now() - 3600000).toISOString()),
		[end, setEnd] = useState("");
	const [query, setQuery] = useState(
			"fields @timestamp, @message, @logStream | sort @timestamp desc | limit 100",
		),
		[limit, setLimit] = useState("100");
	const [id, setId] = useState(""),
		[result, setResult] = useState<QueryResult>(),
		[busy, setBusy] = useState(false),
		[error, setError] = useState("");
	const generation = useRef(0),
		locked = useRef(false),
		queryId = useRef("");
	useEffect(
		() => () => {
			generation.current++;
		},
		[],
	);
	const execute = async (refresh = false) => {
		if (!enabled || locked.current) return;
		locked.current = true;
		setBusy(true);
		setError("");
		const version = ++generation.current;
		try {
			let current = queryId.current;
			if (!refresh) {
				if (!group) throw new Error("Choose a log group.");
				const from = logTime(start, "Start time"),
					to = logTime(end, "End time") ?? Date.now();
				if (from === undefined || from > to)
					throw new Error("Enter a start time before the end time.");
				const text = query.trim();
				if (!text) throw new Error("Enter a query.");
				setResult(undefined);
				setId("");
				current = (
					await logsCall<{ QueryId: string }>(scope, "StartQuery", {
						LogGroupName: group,
						StartTime: Math.floor(from / 1000),
						EndTime: Math.floor(to / 1000),
						QueryString: text,
						Limit: integer(limit, "Result limit", 1, 1000),
					})
				).QueryId;
				if (version !== generation.current) return;
				queryId.current = current;
				setId(current);
			}
			if (!current) throw new Error("Run a query first.");
			const output = await logsCall<QueryResult>(scope, "GetQueryResults", {
				QueryId: current,
			});
			if (version === generation.current) setResult(output);
		} catch (e) {
			if (version === generation.current) setError(errorText(e));
		} finally {
			if (version === generation.current) {
				locked.current = false;
				setBusy(false);
			}
		}
	};
	const rows = (result?.Results ?? []).map((r) =>
			Object.fromEntries(r.map((f) => [f.Field, f.Value])),
		),
		columns = [
			...new Set((result?.Results ?? []).flatMap((r) => r.map((f) => f.Field))),
		];
	return (
		<SpaceBetween size="l">
			<Container
				header={
					<Header
						variant="h2"
						description="Queries run locally to completion. Supported commands include fields, display, filter, parse, sort, limit and stats."
					>
						Logs Insights
					</Header>
				}
			>
				<SpaceBetween size="l">
					{!!error && <Alert type="error">{error}</Alert>}
					<fieldset
						disabled={!enabled || busy}
						style={{ border: 0, margin: 0, padding: 0 }}
					>
						<SpaceBetween size="l">
							<FormField label="Log group">
								<Select
									selectedOption={group ? { value: group, label: group } : null}
									placeholder="Choose a log group"
									options={groups.map((g) => ({
										value: g.name,
										label: g.name,
									}))}
									onChange={({ detail }) =>
										setGroup(detail.selectedOption.value ?? "")
									}
								/>
							</FormField>
							<SpaceBetween direction="horizontal" size="s">
								<FormField label="Start time (ISO 8601)">
									<Input
										ariaLabel="Query start time"
										value={start}
										onChange={({ detail }) => setStart(detail.value)}
									/>
								</FormField>
								<FormField
									label="End time — optional"
									description="Defaults to now. Inclusive at second precision."
								>
									<Input
										ariaLabel="Query end time"
										value={end}
										onChange={({ detail }) => setEnd(detail.value)}
									/>
								</FormField>
							</SpaceBetween>
							<FormField label="Query">
								<Textarea
									ariaLabel="Logs Insights query"
									rows={6}
									value={query}
									onChange={({ detail }) => setQuery(detail.value)}
								/>
							</FormField>
							<FormField label="Result limit" constraintText="1–1,000">
								<Input
									ariaLabel="Query result limit"
									type="number"
									value={limit}
									onChange={({ detail }) => setLimit(detail.value)}
								/>
							</FormField>
						</SpaceBetween>
					</fieldset>
					<SpaceBetween direction="horizontal" size="xs">
						<Button
							variant="primary"
							disabled={!enabled}
							loading={busy}
							onClick={() => execute()}
						>
							Run query
						</Button>
						{!!id && (
							<Button
								disabled={!enabled}
								loading={busy}
								onClick={() => execute(true)}
							>
								Refresh results
							</Button>
						)}
					</SpaceBetween>
				</SpaceBetween>
			</Container>
			{!!id && (
				<KeyValuePairs
					columns={3}
					items={[
						{ label: "Query ID", value: id },
						{ label: "Status", value: result?.Status ?? "Results unavailable" },
						{
							label: "Records matched",
							value: result?.Statistics?.RecordsMatched ?? 0,
						},
						{
							label: "Records scanned",
							value: result?.Statistics?.RecordsScanned ?? 0,
						},
						{
							label: "Bytes scanned",
							value: result?.Statistics?.BytesScanned ?? 0,
						},
					]}
				/>
			)}
			<Table
				items={rows}
				loading={busy}
				wrapLines
				resizableColumns
				columnDefinitions={columns.map((name) => ({
					id: name,
					header: name,
					cell: (r: Record<string, string>) =>
						Object.hasOwn(r, name) ? r[name] : "—",
				}))}
				empty={
					<Box textAlign="center" padding="l">
						{result
							? "No results for this query."
							: "Run a query to load results."}
					</Box>
				}
			/>
		</SpaceBetween>
	);
}
