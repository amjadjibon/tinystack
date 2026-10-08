import { createFileRoute } from "@tanstack/react-router";
import { ConsoleLayout } from "#/components/Console";
import { ResourceTable } from "#/components/ResourceTable";
import { useScope } from "#/lib/scope";
import { service } from "#/lib/services";

export const Route = createFileRoute("/services/$service/")({
	component: ServicePage,
});

function ServicePage() {
	const { service: name } = Route.useParams();
	const {
		account,
		region,
		inventory: { data, loading, reload },
	} = useScope();
	const { title } = service(name);

	return (
		<ConsoleLayout
			breadcrumbs={[{ text: title, href: `/services/${name}` }]}
			content={
				<ResourceTable
					key={`${name}/${account}/${region}`}
					title={title}
					resources={(data ?? []).filter(
						(r) =>
							r.service === name &&
							r.account === account &&
							(!r.region || r.region === "global" || r.region === region),
					)}
					loading={loading}
					onRefresh={reload}
				/>
			}
		/>
	);
}
