import { createFileRoute } from "@tanstack/react-router";
import { Ec2Console } from "#/components/ec2/Ec2Console";
import { ec2Search } from "#/lib/ec2";
export const Route = createFileRoute("/services/ec2")({
	validateSearch: (search: Record<string, unknown>) => ec2Search(search, "ec2"),
	component: () => <Ec2Console area="ec2" search={Route.useSearch()} />,
});
