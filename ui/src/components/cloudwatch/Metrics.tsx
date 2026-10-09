import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	ColumnLayout,
	Container,
	FormField,
	Header,
	Input,
	Pagination,
	Select,
	SpaceBetween,
	Table,
	Textarea,
	TextFilter,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import {
	dimensions,
	dimensionText,
	finite,
	listMetrics,
	type Metric,
	metricKey,
	periodValue,
	statistics,
	type WatchScope,
	watchCall,
} from "#/lib/cloudwatch";
import { usePoll } from "#/lib/usePoll";
import { AlarmForm } from "./AlarmForm";
import { MetricGraph } from "./MetricGraph";

export function Metrics({
	scope,
	enabled,
	onAlarmCreated,
	onChanged,
}: {
	scope: WatchScope;
	enabled: boolean;
	onAlarmCreated: (name: string) => void;
	onChanged: (message: string) => void;
}) {
	const poll = usePoll(
		() => (enabled ? listMetrics(scope) : Promise.resolve([])),
		0,
	);
	const [selected, setSelected] = useState<Metric[]>([]),
		[namespace, setNamespace] = useState(""),
		[stat, setStat] = useState("Average"),
		[period, setPeriod] = useState("60");
	const [graph, setGraph] = useState<{
			selected: Metric[];
			stat: string;
			period: number;
			revision: number;
		}>(),
		[error, setError] = useState("");
	const [creating, setCreating] = useState(false),
		[publishing, setPublishing] = useState(false);
	const [pubNamespace, setPubNamespace] = useState(""),
		[name, setName] = useState(""),
		[dims, setDims] = useState("{}"),
		[value, setValue] = useState("1"),
		[unit, setUnit] = useState("Count");
	const rows = (poll.data ?? []).filter(
		(m) => !namespace || m.Namespace === namespace,
	);
	const collection = useCollection(rows, {
		filtering: {},
		pagination: { pageSize: 20 },
		sorting: {},
	});
	return (
		<SpaceBetween size="l">
			{(poll.error || error) && (
				<Alert type="error">{poll.error || error}</Alert>
			)}
			<Container header={<Header variant="h2">Graphed metrics</Header>}>
				<SpaceBetween size="m">
					<ColumnLayout columns={3}>
						<FormField label="Statistic">
							<Select
								selectedOption={{ value: stat, label: stat }}
								options={statistics.map((value) => ({ value, label: value }))}
								onChange={({ detail }) =>
									setStat(detail.selectedOption.value ?? "Average")
								}
							/>
						</FormField>
						<FormField label="Period (seconds)">
							<Input
								ariaLabel="Metric period"
								value={period}
								onChange={({ detail }) => setPeriod(detail.value)}
							/>
						</FormField>
						<Box padding={{ top: "l" }}>
							<Button
								disabled={!enabled || !selected.length}
								onClick={() => {
									try {
										setGraph({
											selected: [...selected],
											stat,
											period: periodValue(period),
											revision: Date.now(),
										});
										setError("");
									} catch (e) {
										setError(e instanceof Error ? e.message : String(e));
									}
								}}
							>
								Graph selected metrics
							</Button>
						</Box>
					</ColumnLayout>
					{graph ? (
						<MetricGraph
							key={graph.revision}
							scope={scope}
							enabled={enabled}
							autoLoad
							queries={graph.selected.map((Metric, i) => ({
								Id: `m${i}`,
								Label: `${Metric.Namespace}/${Metric.MetricName} ${dimensionText(Metric)} · ${graph.stat}`,
								MetricStat: { Metric, Period: graph.period, Stat: graph.stat },
							}))}
						/>
					) : (
						<Box>Select up to ten metrics below to compare their values.</Box>
					)}
				</SpaceBetween>
			</Container>
			<Table
				{...collection.collectionProps}
				items={collection.items}
				trackBy={metricKey}
				selectionType="multi"
				selectedItems={selected}
				onSelectionChange={({ detail }) => setSelected(detail.selectedItems)}
				isItemDisabled={(m) =>
					selected.length >= 10 &&
					!selected.some((s) => metricKey(s) === metricKey(m))
				}
				loading={poll.loading}
				wrapLines
				header={
					<Header
						counter={`(${rows.length})`}
						description="Each dimension combination is a separate metric."
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									iconName="refresh"
									ariaLabel="Refresh metrics"
									disabled={!enabled}
									onClick={poll.reload}
								/>
								<Button
									disabled={!enabled || selected.length !== 1}
									onClick={() => setCreating(true)}
								>
									Create alarm
								</Button>
								<Button
									variant="primary"
									disabled={!enabled}
									onClick={() => setPublishing(true)}
								>
									Publish metric
								</Button>
							</SpaceBetween>
						}
					>
						All metrics
					</Header>
				}
				filter={
					<SpaceBetween direction="horizontal" size="xs">
						<Select
							selectedOption={{
								value: namespace,
								label: namespace || "All namespaces",
							}}
							options={[
								{ value: "", label: "All namespaces" },
								...[...new Set((poll.data ?? []).map((m) => m.Namespace))]
									.sort()
									.map((value) => ({ value, label: value })),
							]}
							onChange={({ detail }) => {
								setNamespace(detail.selectedOption.value ?? "");
								setSelected([]);
							}}
						/>
						<TextFilter
							{...collection.filterProps}
							filteringPlaceholder="Find metrics"
						/>
					</SpaceBetween>
				}
				pagination={<Pagination {...collection.paginationProps} />}
				columnDefinitions={[
					{
						id: "namespace",
						header: "Namespace",
						sortingField: "Namespace",
						cell: (m) => m.Namespace,
					},
					{
						id: "metric",
						header: "Metric name",
						sortingField: "MetricName",
						cell: (m) => m.MetricName,
					},
					{
						id: "dimensions",
						header: "Dimensions",
						cell: (m) => dimensionText(m) || "None",
					},
				]}
				empty={
					<Box textAlign="center" padding="l">
						No matching metrics. Publish a custom metric or send application
						metric data.
					</Box>
				}
			/>
			{creating && (
				<AlarmForm
					scope={scope}
					metric={selected[0]}
					onClose={() => setCreating(false)}
					onSaved={onAlarmCreated}
				/>
			)}
			{publishing && (
				<ActionDialog
					title="Publish metric"
					submit="Publish metric"
					onClose={() => setPublishing(false)}
					onSaved={() => {
						setPublishing(false);
						poll.reload();
						onChanged("Metric data published.");
					}}
					plan={async () => {
						if (!pubNamespace.trim() || !name.trim())
							throw new Error("Namespace and metric name are required.");
						if (pubNamespace.startsWith("AWS/"))
							throw new Error("Custom namespaces cannot start with AWS/.");
						const metric = {
							MetricName: name,
							Dimensions: dimensions(dims),
							Value: finite(value, "Value"),
							...(unit.trim() ? { Unit: unit.trim() } : {}),
						};
						return [
							{
								label: "Publish metric data",
								run: () =>
									watchCall(scope, "PutMetricData", {
										Namespace: pubNamespace,
										MetricData: [metric],
									}),
							},
						];
					}}
				>
					<SpaceBetween size="l">
						<ColumnLayout columns={2}>
							<FormField label="Namespace">
								<Input
									ariaLabel="Publish metric namespace"
									value={pubNamespace}
									onChange={({ detail }) => setPubNamespace(detail.value)}
								/>
							</FormField>
							<FormField label="Metric name">
								<Input
									ariaLabel="Publish metric name"
									value={name}
									onChange={({ detail }) => setName(detail.value)}
								/>
							</FormField>
						</ColumnLayout>
						<FormField label="Dimensions (JSON)">
							<Textarea
								ariaLabel="Publish metric dimensions"
								value={dims}
								onChange={({ detail }) => setDims(detail.value)}
							/>
						</FormField>
						<ColumnLayout columns={2}>
							<FormField label="Value">
								<Input
									ariaLabel="Metric value"
									value={value}
									onChange={({ detail }) => setValue(detail.value)}
								/>
							</FormField>
							<FormField
								label="Unit"
								description="For example Count, Percent, Milliseconds, Bytes, or None."
							>
								<Input
									ariaLabel="Metric unit"
									value={unit}
									onChange={({ detail }) => setUnit(detail.value)}
								/>
							</FormField>
						</ColumnLayout>
						<Alert type="info">
							Publishes one data point at the current time. Alarm evaluation can
							trigger configured actions.
						</Alert>
					</SpaceBetween>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
