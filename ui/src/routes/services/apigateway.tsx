import { createFileRoute } from "@tanstack/react-router";
import {
	GatewayConsole,
	gatewaySearch,
} from "#/components/apigateway/GatewayConsole";
export const Route = createFileRoute("/services/apigateway")({
	validateSearch: gatewaySearch,
	component: () => (
		<GatewayConsole kind="apigateway" search={Route.useSearch()} />
	),
});
