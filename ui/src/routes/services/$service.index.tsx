import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { ConsoleLayout } from "#/components/Console";
import { GenericResourceActions } from "#/components/GenericResourceActions";
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
		inventory: { data, loading, reload, error },
	} = useScope();
	const { title } = service(name);

	return (
		<ConsoleLayout
			breadcrumbs={[{ text: title, href: `/services/${name}` }]}
			content={
				<SpaceBetween size="l">
					{error && (
						<Alert
							type="error"
							header={
								error.includes("403")
									? "Administrator access required"
									: "Cannot load resources"
							}
						>
							{error}
						</Alert>
					)}
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
						renderActions={(resource) => (
							<GenericResourceActions resource={resource} onChanged={reload} />
						)}
					/>
				</SpaceBetween>
			}
		/>
	);
}
