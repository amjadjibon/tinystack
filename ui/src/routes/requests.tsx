import {
	Badge,
	Button,
	KeyValuePairs,
	SpaceBetween,
} from "@cloudscape-design/components";
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
import { causedDeliveries, deliveryContext, isDenial } from "#/lib/requestLog";

export const Route = createFileRoute("/requests")({ component: Requests });

function Requests() {
	return (
		<LogTable<RequestRecord>
			title="Request inspector"
			description="The last 256 AWS requests and delivery authorization checks. Bodies are truncated, and sensitive operations are redacted."
			fetcher={api.requests}
			filters={[
				{ label: "Deliveries only", matches: (r) => r.method === "DELIVER" },
				{ label: "Denials only", matches: isDenial },
			]}
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
					id: "method",
					header: "Method",
					cell: (r) =>
						r.method === "DELIVER" ? (
							<Badge color="blue">DELIVER</Badge>
						) : (
							r.method
						),
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
				{
					id: "principal",
					header: "Principal / role",
					cell: (r) =>
						deliveryContext(r)?.role || r.authorization?.principal || "-",
				},
				{
					id: "source",
					header: "Source ARN",
					cell: (r) => deliveryContext(r)?.source_arn || "-",
				},
				{
					id: "target",
					header: "Target",
					cell: (r) => (r.method === "DELIVER" ? r.path : "-"),
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
			detailTitle={(r) => `${r.method} ${r.operation || r.path}`}
			detail={(r, { records, select }) => {
				const context = deliveryContext(r);
				const children = causedDeliveries(records, r.request_id);
				const parent = records.find(
					(record) => record.request_id === context?.caused_by,
				);
				return (
					<SpaceBetween size="l">
						<RequestAuthorization decision={r.authorization} />
						{context && (
							<KeyValuePairs
								columns={2}
								items={[
									{
										label: "Service principal",
										value:
											context?.service ||
											context?.principal ||
											r.authorization?.principal ||
											"-",
									},
									{ label: "Role", value: context?.role || "-" },
									{
										label: "Source ARN",
										value: context?.source_arn || "-",
									},
									{
										label: "Source account",
										value: context?.source_account || "-",
									},
									{ label: "Target", value: r.path },
									{
										label: "Caused by request",
										value: (() => {
											const id = context.caused_by;
											return parent ? (
												<Button
													variant="inline-link"
													onClick={() => select(parent)}
												>
													{id}
												</Button>
											) : id ? (
												`${id} (no longer retained)`
											) : (
												"No originating request recorded (background delivery)"
											);
										})(),
									},
								]}
							/>
						)}
						<KeyValuePairs
							items={[
								{
									label: "Deliveries caused (retained checks)",
									value: (
										<SpaceBetween size="xs">
											{children.map((delivery) => (
												<Button
													key={delivery.request_id}
													variant="inline-link"
													onClick={() => select(delivery)}
												>
													{delivery.service}:{delivery.operation} —{" "}
													{authorizationLabel(delivery.authorization)} —{" "}
													{delivery.request_id}
												</Button>
											))}
											{!children.length && "None in the retained log"}
										</SpaceBetween>
									),
								},
							]}
						/>
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
				);
			}}
		/>
	);
}
