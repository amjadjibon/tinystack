import {
	ExpandableSection,
	KeyValuePairs,
	ProgressBar,
} from "@cloudscape-design/components";
import { CodeBlock } from "#/components/CodeBlock";
import { type Definition, period } from "#/lib/cost";

import { costPercentage } from "#/lib/costUsage";

type Money = { Amount: string; Unit: string };
function spend(config: Definition) {
	const actual = (config.CalculatedSpend as { ActualSpend?: Money } | undefined)
		?.ActualSpend;
	const planned = config.PlannedBudgetLimits as
		| Record<string, Money>
		| undefined;
	const active =
		planned &&
		Object.entries(planned)
			.filter(([epoch]) => Number(epoch) <= Date.now() / 1000)
			.sort(([a], [b]) => Number(b) - Number(a))[0]?.[1];
	const limit = (config.BudgetLimit as Money | undefined) ?? active;
	const percent =
		actual && limit && actual.Unit === limit.Unit
			? costPercentage(actual.Amount, limit.Amount)
			: undefined;
	return { actual, limit, percent };
}
const money = (value?: Money) =>
	value ? `${value.Amount} ${value.Unit}` : "—";
export function BudgetSpend({ config }: { config: Definition }) {
	const { actual, limit, percent } = spend(config);
	if (!actual)
		return (
			<span title="Actual spend is available for active supported COST budgets in USD with monthly, quarterly or annual periods, supported dimension filters and no explicit CostTypes.">
				— (spend unavailable for this configuration)
			</span>
		);
	return (
		<ProgressBar
			value={Math.max(0, Math.min(100, Number(percent ?? 0)))}
			label={`${money(actual)} / ${money(limit)}`}
			description={
				percent === undefined
					? "Percentage unavailable"
					: `${percent}% used${Number(percent) > 100 ? " (over budget)" : ""}`
			}
		/>
	);
}
export function BudgetDetails({ config }: { config: Definition }) {
	const { actual, limit, percent } = spend(config);
	const planned = config.PlannedBudgetLimits as
		| Record<string, Money>
		| undefined;
	const filters = config.CostFilters as Record<string, string[]> | undefined;
	return (
		<>
			<KeyValuePairs
				columns={2}
				items={[
					{ label: "Type", value: String(config.BudgetType ?? "—") },
					{ label: "Time unit", value: String(config.TimeUnit ?? "—") },
					{ label: "Limit (current)", value: money(limit) },
					{ label: "Period (UTC)", value: period(config.TimePeriod) },
					{
						label: "Actual spend",
						value: actual ? money(actual) : <BudgetSpend config={config} />,
					},
					{
						label: "Percentage used",
						value: percent === undefined ? "—" : `${percent}%`,
					},
					{
						label: "Cost filters",
						value:
							filters && Object.keys(filters).length
								? Object.entries(filters)
										.map(([key, values]) => `${key}: ${values.join(", ")}`)
										.join("; ")
								: "All costs",
					},
					...(planned
						? [
								{
									label: "Planned limits",
									value: Object.entries(planned)
										.sort(([a], [b]) => Number(a) - Number(b))
										.map(
											([epoch, value]) =>
												`${new Date(Number(epoch) * 1000).toISOString().slice(0, 10)}: ${money(value)}`,
										)
										.join("; "),
								},
							]
						: []),
				]}
			/>
			<ExpandableSection headerText="JSON">
				<CodeBlock text={JSON.stringify(config, null, 2)} />
			</ExpandableSection>
		</>
	);
}
