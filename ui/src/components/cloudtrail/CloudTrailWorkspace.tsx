import { Tabs } from "@cloudscape-design/components";
import type { CloudTrailScope } from "#/lib/cloudtrail";
import { EventHistory } from "./EventHistory";
import { TrailsTable } from "./TrailsTable";
export function CloudTrailWorkspace(props: {
	scope: CloudTrailScope;
	enabled: boolean;
	onChanged: () => void;
}) {
	return (
		<Tabs
			tabs={[
				{
					id: "history",
					label: "Event history",
					content: <EventHistory {...props} />,
				},
				{ id: "trails", label: "Trails", content: <TrailsTable {...props} /> },
			]}
		/>
	);
}
