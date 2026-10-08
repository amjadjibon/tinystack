import { ContentLayout, Header } from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { ConsoleLayout } from "#/components/Console";
import { ServiceCatalog } from "#/components/ServiceCatalog";
import { useHealth } from "#/lib/health";
import { useScope } from "#/lib/scope";

export const Route = createFileRoute("/services/")({ component: AllServices });

function AllServices() {
	const health = useHealth();
	const { inventory } = useScope();
	return (
		<ConsoleLayout
			breadcrumbs={[{ text: "All services", href: "/services" }]}
			contentType="default"
			content={
				<ContentLayout header={<Header variant="h1">All services</Header>}>
					<ServiceCatalog
						names={health.data?.services ?? []}
						resources={inventory.data ?? []}
					/>
				</ContentLayout>
			}
		/>
	);
}
