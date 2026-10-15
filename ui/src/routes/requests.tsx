import { KeyValuePairs, SpaceBetween } from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import {
	AuthorizationStatus,
	authorizationLabel,
	RequestAuthorization,
} from "#/components/Authorization";
import { CodeBlock } from "#/components/CodeBlock";
import { LogTable } from "#/components/LogTable";
import { RequestStatus } from "#/components/RequestStatus";
import { ServiceLabel } from "#/components/ServiceIcon";
import { api, pretty, type RequestRecord } from "#/lib/api";

export const Route = createFileRoute("/requests")({ component: Requests });

function Requests() {
	return (
		<LogTable<RequestRecord>
			title="Request inspector"
			description="The last 256 AWS requests TinyStack served. Bodies are truncated, and sensitive operations are redacted."
			fetcher={api.requests}
			filterText={(r) =>
				[
					authorizationLabel(r.authorization),
					...Object.values(r).filter((value) => typeof value !== "object"),
					r.authorization?.principal,
					r.authorization?.mode,
					r.authorization?.message,
					...(r.authorization?.checks ?? []).flatMap((check) => [
						check.action,
						check.resource,
						...(check.matched ?? []),
						...(check.resource_policy?.matched ?? []),
						...(check.unsupported ?? []),
					]),
				].join(" ")
			}
			onClear={() => api.clear("requests")}
			trackBy={(r) => r.request_id}
			empty="Send a request with the AWS CLI or an SDK to see it here."
			columns={[
				{
					id: "time",
					header: "Time",
					cell: (r) => new Date(r.time).toLocaleTimeString(),
					width: 130,
					sortingField: "time",
				},
				{
					id: "service",
					header: "Service",
					cell: (r) => (r.service ? <ServiceLabel name={r.service} /> : "-"),
					width: 240,
					sortingField: "service",
				},
				{
					id: "operation",
					header: "Operation",
					cell: (r) => r.operation || `${r.method} ${r.path}`,
					width: 220,
					sortingField: "operation",
					isRowHeader: true,
				},
				{
					id: "status",
					header: "Status",
					cell: (r) => <RequestStatus status={r.status} />,
					sortingField: "status",
				},
				{ id: "error", header: "Error", cell: (r) => r.error_code || "-" },
				{
					id: "authorization",
					header: "IAM decision",
					cell: (r) => <AuthorizationStatus decision={r.authorization} />,
					width: 160,
				},
				{ id: "account", header: "Account", cell: (r) => r.account },
				{ id: "region", header: "Region", cell: (r) => r.region },
				{
					id: "duration",
					header: "Duration (ms)",
					cell: (r) => r.duration_ms.toFixed(1),
					sortingField: "duration_ms",
				},
			]}
			detailTitle={(r) => r.operation || `${r.method} ${r.path}`}
			detail={(r) => (
				<SpaceBetween size="l">
					<RequestAuthorization decision={r.authorization} />
					<KeyValuePairs
						columns={2}
						items={[
							{ label: "Request ID", value: r.request_id },
							{ label: "Status", value: <RequestStatus status={r.status} /> },
							{ label: "Method and path", value: `${r.method} ${r.path}` },
							{ label: "Protocol", value: r.protocol || "-" },
							{ label: "Account", value: r.account },
							{ label: "Region", value: r.region },
							{ label: "Time", value: new Date(r.time).toLocaleString() },
							{ label: "Duration", value: `${r.duration_ms.toFixed(1)} ms` },
							...(r.error_code
								? [
										{
											label: "Error",
											value: `${r.error_code}: ${r.error_message ?? ""}`,
										},
									]
								: []),
						]}
					/>
					<KeyValuePairs
						items={[
							{
								label: `Request body (${r.request_size} bytes)`,
								value: <CodeBlock text={pretty(r.request_body)} />,
							},
							{
								label: `Response body (${r.response_size} bytes)`,
								value: <CodeBlock text={pretty(r.response_body)} />,
							},
						]}
					/>
				</SpaceBetween>
			)}
		/>
	);
}
