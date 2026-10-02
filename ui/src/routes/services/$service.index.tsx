import { createFileRoute } from "@tanstack/react-router";
import { ConsoleLayout } from "#/components/Console";
import { ResourceTable } from "#/components/ResourceTable";
import { api } from "#/lib/api";
import { service } from "#/lib/services";
import { usePoll } from "#/lib/usePoll";

export const Route = createFileRoute("/services/$service/")({
	component: ServicePage,
});

function ServicePage() {
	const { service: name } = Route.useParams();
	const { data, loading, reload } = usePoll(api.resources);
	const { title } = service(name);

	return (
		<ConsoleLayout
			breadcrumbs={[{ text: title, href: `/services/${name}` }]}
			content={
				<ResourceTable
					key={name}
					title={title}
					resources={(data ?? []).filter((r) => r.service === name)}
					loading={loading}
					onRefresh={reload}
				/>
			}
		/>
	);
}
