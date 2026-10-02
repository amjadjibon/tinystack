import { createFileRoute } from "@tanstack/react-router";
import { LogTable } from "#/components/LogTable";
import { RequestStatus } from "#/components/RequestStatus";
import { ServiceLabel } from "#/components/ServiceIcon";
import { api, type MetricSeries } from "#/lib/api";

export const Route = createFileRoute("/metrics")({ component: Metrics });

function Metrics() {
	return (
		<LogTable<MetricSeries>
			title="Metrics"
			description="Request counts and latency by service, operation, and status since TinyStack started."
			fetcher={api.metrics}
			trackBy={(m) => `${m.service} ${m.operation} ${m.status}`}
			empty="Metrics appear once TinyStack serves a request."
			columns={[
				{
					id: "service",
					header: "Service",
					cell: (m) => <ServiceLabel name={m.service} />,
					sortingField: "service",
				},
				{
					id: "operation",
					header: "Operation",
					cell: (m) => m.operation || "(unknown)",
					sortingField: "operation",
					isRowHeader: true,
				},
				{
					id: "status",
					header: "Status",
					cell: (m) => <RequestStatus status={m.status} />,
					sortingField: "status",
				},
				{
					id: "count",
					header: "Requests",
					cell: (m) => m.count,
					sortingField: "count",
				},
				{
					id: "avg",
					header: "Average (ms)",
					cell: (m) =>
						m.count ? ((m.seconds / m.count) * 1000).toFixed(1) : "-",
				},
			]}
		/>
	);
}
