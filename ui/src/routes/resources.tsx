import { createFileRoute } from "@tanstack/react-router";
import { ConsoleLayout } from "#/components/Console";
import { ResourceTable } from "#/components/ResourceTable";
import { api } from "#/lib/api";
import { usePoll } from "#/lib/usePoll";

export const Route = createFileRoute("/resources")({ component: Resources });

function Resources() {
	const { data, loading, reload } = usePoll(api.resources);
	return (
		<ConsoleLayout
			breadcrumbs={[{ text: "All resources", href: "/resources" }]}
			content={
				<ResourceTable
					title="All resources"
					resources={data ?? []}
					loading={loading}
					onRefresh={reload}
					showService
				/>
			}
		/>
	);
}
