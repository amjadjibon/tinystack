import {
	Alert,
	Box,
	Button,
	ColumnLayout,
	FormField,
	Input,
	LineChart,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useEffect, useRef, useState } from "react";
import {
	lastHour,
	type MetricQuery,
	type MetricResult,
	timeRange,
	timestamp,
	type WatchScope,
	watchCall,
} from "#/lib/cloudwatch";
import { errorText } from "#/lib/consoleActions";

export function MetricGraph({
	scope,
	queries,
	enabled,
	threshold,
	autoLoad = false,
}: {
	scope: WatchScope;
	queries: MetricQuery[];
	enabled: boolean;
	threshold?: number;
	autoLoad?: boolean;
}) {
	const [range] = useState(lastHour),
		[start, setStart] = useState(range[0] as string),
		[end, setEnd] = useState(range[1] as string);
	const [result, setResult] = useState<MetricResult[]>(),
		[error, setError] = useState(""),
		[busy, setBusy] = useState(false),
		[loadedRange, setLoadedRange] = useState<{
			StartTime: number;
			EndTime: number;
		}>();
	const generation = useRef(0),
		locked = useRef(false);
	// Read results belong to the mounted scope and query selection.
	const run = async (s = start, e = end) => {
		if (!enabled || locked.current) return;
		locked.current = true;
		const id = ++generation.current;
		setBusy(true);
		setError("");
		setResult(undefined);
		try {
			if (!queries.length)
				throw new Error("Select at least one metric to graph.");
			const range = timeRange(s, e);
			const out = await watchCall<{
				MetricDataResults?: MetricResult[];
				Messages?: { Value?: string }[];
			}>(scope, "GetMetricData", {
				MetricDataQueries: queries,
				...range,
				ScanBy: "TimestampAscending",
				MaxDatapoints: 10000,
			});
			if (generation.current === id) {
				setResult(out.MetricDataResults ?? []);
				setLoadedRange(range);
				if (out.Messages?.length)
					setError(out.Messages.map((m) => m.Value).join("; "));
			}
		} catch (err) {
			if (generation.current === id) setError(errorText(err));
		} finally {
			if (generation.current === id) {
				setBusy(false);
				locked.current = false;
			}
		}
	};
	// biome-ignore lint/correctness/useExhaustiveDependencies: keyed by scope, queries and availability
	useEffect(() => {
		locked.current = false;
		if (autoLoad) void run();
		return () => {
			generation.current++;
			locked.current = false;
		};
		// Parent keys this component by scope, queries and availability.
	}, []);
	const series = (result ?? []).map((r) => ({
		type: "line" as const,
		title: r.Label || r.Id,
		data: (r.Timestamps ?? [])
			.map((t, i) => ({ x: new Date(t * 1000), y: r.Values?.[i] ?? NaN }))
			.filter((d) => Number.isFinite(d.y))
			.sort((a, b) => a.x.getTime() - b.x.getTime()),
	}));
	const rows = (result ?? []).map((r) => ({
		id: r.Id,
		name: r.Label || r.Id,
		status: r.StatusCode,
		count: r.Values?.length ?? 0,
		latest: r.Values?.at(-1),
		at: r.Timestamps?.at(-1),
	}));
	return (
		<SpaceBetween size="m">
			<ColumnLayout columns={3}>
				<FormField label="Start time" description="ISO 8601">
					<Input
						ariaLabel="Metric graph start time"
						value={start}
						onChange={({ detail }) => setStart(detail.value)}
					/>
				</FormField>
				<FormField label="End time" description="Exclusive, ISO 8601">
					<Input
						ariaLabel="Metric graph end time"
						value={end}
						onChange={({ detail }) => setEnd(detail.value)}
					/>
				</FormField>
				<Box padding={{ top: "l" }}>
					<SpaceBetween direction="horizontal" size="xs">
						<Button
							disabled={!enabled}
							loading={busy}
							onClick={() => void run()}
						>
							Graph metrics
						</Button>
						<Button
							disabled={!enabled || busy}
							onClick={() => {
								const [s, e] = lastHour();
								setStart(s);
								setEnd(e);
								void run(s, e);
							}}
						>
							Last hour
						</Button>
					</SpaceBetween>
				</Box>
			</ColumnLayout>
			{error && <Alert type="error">{error}</Alert>}
			{result?.some((r) => r.StatusCode !== "Complete") && (
				<Alert type="warning">
					Some series are incomplete. Narrow the time range or increase the
					period to stay within 10,000 data points.
				</Alert>
			)}
			{result && (
				<Box color="text-body-secondary">
					Showing {timestamp(loadedRange?.StartTime)} to{" "}
					{timestamp(loadedRange?.EndTime)}.
				</Box>
			)}
			<LineChart<Date>
				series={[
					...series,
					...(threshold === undefined
						? []
						: [
								{
									type: "threshold" as const,
									title: "Alarm threshold",
									y: threshold,
								},
							]),
				]}
				xScaleType="time"
				xDomain={
					loadedRange
						? [
								new Date(loadedRange.StartTime * 1000),
								new Date(loadedRange.EndTime * 1000),
							]
						: undefined
				}
				height={260}
				statusType={busy ? "loading" : "finished"}
				ariaLabel="CloudWatch metric graph"
				xTitle="Time"
				yTitle="Value"
				empty={
					<Box textAlign="center">
						No data points. Select metrics and a time range, then graph metrics.
					</Box>
				}
				i18nStrings={{
					xTickFormatter: (t) => t.toLocaleTimeString(),
					yTickFormatter: (v) => v.toLocaleString(),
				}}
			/>
			{result && (
				<Table
					items={rows}
					variant="embedded"
					trackBy="id"
					columnDefinitions={[
						{ id: "name", header: "Series", cell: (r) => r.name },
						{ id: "status", header: "Status", cell: (r) => r.status ?? "—" },
						{ id: "points", header: "Data points", cell: (r) => r.count },
						{
							id: "latest",
							header: "Latest value",
							cell: (r) => r.latest ?? "—",
						},
						{
							id: "time",
							header: "Latest timestamp",
							cell: (r) => timestamp(r.at),
						},
					]}
					empty={<Box>No series returned.</Box>}
				/>
			)}
		</SpaceBetween>
	);
}
