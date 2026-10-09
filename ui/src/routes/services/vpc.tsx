import { createFileRoute } from "@tanstack/react-router";
import { Ec2Console } from "#/components/ec2/Ec2Console";
import { ec2Search } from "#/lib/ec2";
export const Route = createFileRoute("/services/vpc")({
	validateSearch: (search: Record<string, unknown>) => ec2Search(search, "vpc"),
	component: () => <Ec2Console area="vpc" search={Route.useSearch()} />,
});
