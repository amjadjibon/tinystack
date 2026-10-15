import { createFileRoute } from "@tanstack/react-router";
import { CostPage } from "#/components/cost/CostPage";
export const Route = createFileRoute("/services/costandusagereportservice")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		name: String(search.name ?? ""),
		section: "report-definition",
	}),
	component: Page,
});
function Page() {
	const search = Route.useSearch();
	return (
		<CostPage
			service="costandusagereportservice"
			section={search.section}
			name={search.name}
		/>
	);
}
