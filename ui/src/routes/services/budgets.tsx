import { createFileRoute } from "@tanstack/react-router";
import { CostPage } from "#/components/cost/CostPage";
export const Route = createFileRoute("/services/budgets")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		name: String(search.name ?? ""),
		section: "budget",
	}),
	component: Page,
});
function Page() {
	const search = Route.useSearch();
	return (
		<CostPage service="budgets" section={search.section} name={search.name} />
	);
}
