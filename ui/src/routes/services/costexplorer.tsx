import { createFileRoute } from "@tanstack/react-router";
import { CostPage } from "#/components/cost/CostPage";
export const Route = createFileRoute("/services/costexplorer")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		name: String(search.name ?? ""),
		section: [
			"usage",
			"data",
			"anomalymonitor",
			"anomalysubscription",
			"costcategory",
		].includes(String(search.section))
			? String(search.section)
			: "usage",
	}),
	component: Page,
});
function Page() {
	const search = Route.useSearch();
	return (
		<CostPage
			service="costexplorer"
			section={search.section}
			name={search.name}
		/>
	);
}
