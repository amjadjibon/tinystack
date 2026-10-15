import {
	Alert,
	BarChart,
	Box,
	Button,
	Container,
	DatePicker,
	FormField,
	Header,
	Link,
	Select,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useEffect, useState } from "react";
import { costCall, type Definition } from "#/lib/cost";
import {
	type CostPeriod,
	costPreset,
	decimalUnits,
	sumCosts,
	usageRows,
} from "#/lib/costUsage";

export async function costPages(
	account: string,
	operation: string,
	input: Definition,
) {
	const pages: Definition[] = [];
	let token: string | undefined;
	do {
		const page = (await costCall("costexplorer", account, operation, {
			...input,
			...(token ? { NextPageToken: token } : {}),
		})) as Definition;
		pages.push(page);
		token = page.NextPageToken as string | undefined;
	} while (token);
	return pages;
}
const options = (values: string[]) =>
	values.map((value) => ({ value, label: value }));
export function CostUsage({
	account,
	enabled,
}: {
	account: string;
	enabled: boolean;
}) {
	const [range, setRange] = useState(() => costPreset("month"));
	const [preset, setPreset] = useState("month");
	const [granularity, setGranularity] = useState("DAILY");
	const [group, setGroup] = useState("");
	const [service, setService] = useState("");
	const [region, setRegion] = useState("");
	const [dimensions, setDimensions] = useState<{
		services: string[];
		regions: string[];
		tags: string[];
	}>({ services: [], regions: [], tags: [] });
	const [periods, setPeriods] = useState<CostPeriod[]>([]);
	const [error, setError] = useState("");
	const [loading, setLoading] = useState(false);
	useEffect(() => {
		if (!enabled || !range.Start || !range.End || range.Start >= range.End)
			return;
		let current = true;
		setLoading(true);
		setError("");
		setPeriods([]);
		const filters = [
			["SERVICE", service],
			["REGION", region],
		]
			.filter(([, value]) => value)
			.map(([Key, value]) => ({ Dimensions: { Key, Values: [value] } }));
		const input = {
			TimePeriod: range,
			Granularity: granularity,
			Metrics: ["UnblendedCost"],
			...(group
				? {
						GroupBy: [
							{
								Type: group.startsWith("tag:") ? "TAG" : "DIMENSION",
								Key: group.replace(/^tag:/, ""),
							},
						],
					}
				: {}),
			...(filters.length
				? { Filter: filters.length === 1 ? filters[0] : { And: filters } }
				: {}),
		};
		Promise.all([
			costPages(account, "GetCostAndUsage", input),
			costPages(account, "GetDimensionValues", {
				TimePeriod: range,
				Dimension: "SERVICE",
			}),
			costPages(account, "GetDimensionValues", {
				TimePeriod: range,
				Dimension: "REGION",
			}),
			costPages(account, "GetTags", { TimePeriod: range }),
		])
			.then(([usage, services, regions, tags]) => {
				if (!current) return;
				setPeriods(usage.flatMap((p) => p.ResultsByTime as CostPeriod[]));
				const values = (pages: Definition[]) =>
					pages.flatMap((p) =>
						((p.DimensionValues as { Value: string }[] | undefined) ?? []).map(
							(v) => v.Value,
						),
					);
				setDimensions({
					services: values(services),
					regions: values(regions),
					tags: tags.flatMap((p) => (p.Tags as string[] | undefined) ?? []),
				});
			})
			.catch((e) => {
				if (current) setError(String(e));
			})
			.finally(() => {
				if (current) setLoading(false);
			});
		return () => {
			current = false;
		};
	}, [account, enabled, range, granularity, group, service, region]);
	const rows = usageRows(periods);
	const keys = [...new Set(rows.flatMap((r) => Object.keys(r.amounts)))];
	const ranked = keys
		.map((key) => ({
			key,
			total: decimalUnits(sumCosts(rows.map((r) => r.amounts[key] ?? "0"))),
		}))
		.sort((a, b) => {
			const x = a.total < 0n ? -a.total : a.total,
				y = b.total < 0n ? -b.total : b.total;
			return x > y ? -1 : x < y ? 1 : a.key.localeCompare(b.key);
		});
	const top = ranked.slice(0, 9).map((r) => r.key);
	const rest = ranked.slice(9).map((r) => r.key);
	const columns = [
		...top.map((key) => ({ key, label: key, values: [key] })),
		...(rest.length ? [{ key: "__other", label: "Other", values: rest }] : []),
	];
	const items = rows.map((r) => ({
		period: `${r.start} – ${r.end}${r.estimated ? " (estimated)" : ""}`,
		values: columns.map((c) =>
			sumCosts(c.values.map((key) => r.amounts[key] ?? "0")),
		),
		total: sumCosts(Object.values(r.amounts)),
	}));
	return (
		<SpaceBetween size="l">
			<Header
				variant="h1"
				actions={
					<Button
						disabled={!enabled}
						onClick={() => setRange((r) => ({ ...r }))}
					>
						Refresh costs
					</Button>
				}
			>
				Cost and usage
			</Header>
			<Container>
				<SpaceBetween size="m" direction="horizontal">
					<FormField label="Date preset">
						<Select
							selectedOption={{
								value: preset,
								label: {
									month: "This month",
									"7days": "Last 7 days",
									lastmonth: "Last month",
									"3months": "Last 3 months",
									custom: "Custom",
								}[preset],
							}}
							options={[
								{ value: "month", label: "This month" },
								{ value: "7days", label: "Last 7 days" },
								{ value: "lastmonth", label: "Last month" },
								{ value: "3months", label: "Last 3 months" },
							]}
							onChange={({ detail }) => {
								const value = detail.selectedOption.value ?? "";
								setPreset(value);
								setRange(costPreset(value));
							}}
						/>
					</FormField>
					<FormField label="Start (UTC, inclusive)">
						<DatePicker
							value={range.Start}
							onChange={({ detail }) => {
								setPreset("custom");
								setRange((r) => ({ ...r, Start: detail.value }));
							}}
						/>
					</FormField>
					<FormField label="End (UTC, exclusive)">
						<DatePicker
							value={range.End}
							onChange={({ detail }) => {
								setPreset("custom");
								setRange((r) => ({ ...r, End: detail.value }));
							}}
						/>
					</FormField>
					<FormField label="Granularity">
						<Select
							selectedOption={{ value: granularity }}
							options={options(["DAILY", "MONTHLY"])}
							onChange={({ detail }) =>
								setGranularity(detail.selectedOption.value ?? "")
							}
						/>
					</FormField>
				</SpaceBetween>
			</Container>
			<Container>
				<SpaceBetween size="m" direction="horizontal">
					<FormField label="Group by">
						<Select
							selectedOption={{ value: group, label: group || "None" }}
							options={[
								{ value: "", label: "None" },
								...[
									{ value: "SERVICE", label: "Service" },
									{ value: "REGION", label: "Region" },
									{ value: "USAGE_TYPE", label: "Usage type" },
									{ value: "LINKED_ACCOUNT", label: "Linked account" },
								],
								...dimensions.tags.map((t) => ({
									value: `tag:${t}`,
									label: `Tag: ${t}`,
								})),
							]}
							onChange={({ detail }) =>
								setGroup(detail.selectedOption.value ?? "")
							}
						/>
					</FormField>
					<FormField label="Service filter">
						<Select
							selectedOption={{
								value: service,
								label: service || "All services",
							}}
							options={[
								{ value: "", label: "All services" },
								...options(dimensions.services),
							]}
							onChange={({ detail }) =>
								setService(detail.selectedOption.value ?? "")
							}
						/>
					</FormField>
					<FormField label="Region filter">
						<Select
							selectedOption={{ value: region, label: region || "All regions" }}
							options={[
								{ value: "", label: "All regions" },
								...options(dimensions.regions),
							]}
							onChange={({ detail }) =>
								setRegion(detail.selectedOption.value ?? "")
							}
						/>
					</FormField>
				</SpaceBetween>
			</Container>
			{range.Start >= range.End && (
				<Alert type="error">End must be after start.</Alert>
			)}
			{error && <Alert type="error">{error}</Alert>}
			{!loading &&
				!error &&
				(!keys.length ||
					rows.every((r) =>
						Object.values(r.amounts).every((v) => decimalUnits(v) === 0n),
					)) && (
					<Alert>
						Costs come from loaded cost data. Load a fixture in Cost data to
						explore spend.{" "}
						<Link
							href="https://amjadjibon.github.io/tinystack/guides/costs/"
							external
						>
							Costs guide
						</Link>
					</Alert>
				)}
			<Container header={<Header variant="h2">Unblended cost (USD)</Header>}>
				<BarChart
					hideFilter
					i18nStrings={{
						legendAriaLabel: "Cost groups",
						chartAriaRoleDescription: "Stacked bar chart",
						xTickFormatter: (value) => String(value),
						yTickFormatter: (value) => value.toLocaleString(),
					}}
					height={300}
					statusType={loading ? "loading" : error ? "error" : "finished"}
					xScaleType="categorical"
					stackedBars
					xTitle="Period start (UTC)"
					yTitle="USD"
					series={columns.map((c) => ({
						title: c.label,
						type: "bar" as const,
						data: rows.map((r) => ({
							x: r.start,
							y: Number(sumCosts(c.values.map((key) => r.amounts[key] ?? "0"))),
						})),
					}))}
					ariaLabel="Cost and usage by period"
					empty={<Box>No cost data</Box>}
					noMatch={<Box>No matching costs</Box>}
				/>
			</Container>
			<Table
				loading={loading}
				items={
					items.length
						? [
								...items,
								{
									period: "Total",
									values: columns.map((_, i) =>
										sumCosts(items.map((r) => r.values[i])),
									),
									total: sumCosts(items.map((r) => r.total)),
								},
							]
						: []
				}
				header={<Header variant="h2">Cost table (USD)</Header>}
				columnDefinitions={[
					{
						id: "period",
						header: "Period (exclusive end)",
						cell: (r) => r.period,
					},
					...columns.map((c, i) => ({
						id: c.key,
						header: c.label,
						cell: (r: (typeof items)[number]) => r.values[i],
					})),
					{ id: "total", header: "Total", cell: (r) => r.total },
				]}
				empty="No costs for this range"
			/>
		</SpaceBetween>
	);
}
