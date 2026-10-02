import {
	KeyValuePairs,
	SpaceBetween,
	StatusIndicator,
} from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { CodeBlock } from "#/components/CodeBlock";
import { LogTable } from "#/components/LogTable";
import { api, type Delivery, pretty } from "#/lib/api";

export const Route = createFileRoute("/deliveries")({ component: Deliveries });

function DeliveryStatus({ status }: { status: string }) {
	return (
		<StatusIndicator type={status === "delivered" ? "success" : "error"}>
			{status}
		</StatusIndicator>
	);
}

function Deliveries() {
	return (
		<LogTable<Delivery>
			title="Deliveries"
			description="Messages SNS, S3, EventBridge, Lambda, and Cognito delivered in-process, newest first."
			fetcher={api.deliveries}
			onClear={() => api.clear("deliveries")}
			trackBy={(d) => `${d.time} ${d.via} ${d.target}`}
			empty="Publish to a topic or put an event with a target to see deliveries here."
			columns={[
				{
					id: "time",
					header: "Time",
					cell: (d) => new Date(d.time).toLocaleTimeString(),
					sortingField: "time",
				},
				{ id: "via", header: "Via", cell: (d) => d.via, sortingField: "via" },
				{ id: "source", header: "Source", cell: (d) => d.source || "-" },
				{
					id: "target",
					header: "Target",
					cell: (d) => d.target,
					isRowHeader: true,
				},
				{
					id: "status",
					header: "Status",
					cell: (d) => <DeliveryStatus status={d.status} />,
					sortingField: "status",
				},
				{ id: "error", header: "Error", cell: (d) => d.error || "-" },
			]}
			detailTitle={(d) => d.target}
			detail={(d) => (
				<SpaceBetween size="l">
					<KeyValuePairs
						columns={2}
						items={[
							{ label: "Via", value: d.via },
							{ label: "Status", value: <DeliveryStatus status={d.status} /> },
							{ label: "Source", value: d.source || "(direct invocation)" },
							{ label: "Target", value: d.target },
							{ label: "Time", value: new Date(d.time).toLocaleString() },
							...(d.error ? [{ label: "Error", value: d.error }] : []),
						]}
					/>
					<KeyValuePairs
						items={[
							{
								label: `Payload (${d.size} bytes)`,
								value: <CodeBlock text={pretty(d.payload)} />,
							},
						]}
					/>
				</SpaceBetween>
			)}
		/>
	);
}
