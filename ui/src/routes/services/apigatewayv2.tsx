import { createFileRoute } from "@tanstack/react-router";
import {
	GatewayConsole,
	gatewaySearch,
} from "#/components/apigateway/GatewayConsole";
export const Route = createFileRoute("/services/apigatewayv2")({
	validateSearch: gatewaySearch,
	component: () => (
		<GatewayConsole kind="apigatewayv2" search={Route.useSearch()} />
	),
});
