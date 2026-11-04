import {
	Alert,
	Box,
	Button,
	FormField,
	Header,
	Input,
	Select,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import {
	type HealthCheck,
	type Route53Scope,
	route53Call,
	route53Pages,
} from "#/lib/route53";
import {
	buildHealthCheckConfig,
	emptyHealthCheckForm,
	type HealthCheckForm,
	healthCheckTypes,
} from "#/lib/route53Helpers";
import { usePoll } from "#/lib/usePoll";

export function HealthChecks({
	scope,
	enabled,
	onChanged,
}: {
	scope: Route53Scope;
	enabled: boolean;
	onChanged: () => void;
}) {
	const checks = usePoll(
		() =>
			enabled
				? route53Pages<HealthCheck>(scope, "ListHealthChecks", "HealthChecks")
				: Promise.resolve([]),
		5000,
	);
	const [selected, setSelected] = useState<HealthCheck[]>([]);
	const [dialog, setDialog] = useState<"create" | "delete" | null>(null);
	const current = selected[0];
	const done = () => {
		setDialog(null);
		setSelected([]);
		checks.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="m">
			{!!checks.error && (
				<Alert type="error" header="Cannot load health checks">
					{checks.error}
				</Alert>
			)}
			<Table
				items={checks.data ?? []}
				trackBy="Id"
				loading={checks.loading}
				loadingText="Loading health checks"
				selectionType="single"
				selectedItems={selected}
				onSelectionChange={({ detail }) => setSelected(detail.selectedItems)}
				header={
					<Header
						variant="h2"
						counter={`(${checks.data?.length ?? 0})`}
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									iconName="refresh"
									ariaLabel="Refresh health checks"
									onClick={checks.reload}
								/>
								<Button
									disabled={!enabled || !current}
									onClick={() => setDialog("delete")}
								>
									Delete health check
								</Button>
								<Button
									variant="primary"
									disabled={!enabled}
									onClick={() => setDialog("create")}
								>
									Create health check
								</Button>
							</SpaceBetween>
						}
					>
						Health checks
					</Header>
				}
				empty={
					<Box>
						No health checks. Create one to monitor an endpoint from record
						sets.
					</Box>
				}
				columnDefinitions={[
					{ id: "id", header: "ID", cell: (h) => h.Id, isRowHeader: true },
					{ id: "type", header: "Type", cell: (h) => h.HealthCheckConfig.Type },
					{
						id: "target",
						header: "Target",
						cell: (h) =>
							h.HealthCheckConfig.FullyQualifiedDomainName ||
							h.HealthCheckConfig.IPAddress ||
							"—",
					},
					{
						id: "port",
						header: "Port",
						cell: (h) => h.HealthCheckConfig.Port ?? "—",
					},
					{
						id: "path",
						header: "Path",
						cell: (h) => h.HealthCheckConfig.ResourcePath ?? "—",
					},
					{
						id: "interval",
						header: "Interval (s)",
						cell: (h) => h.HealthCheckConfig.RequestInterval ?? "—",
					},
				]}
			/>
			{dialog === "create" && (
				<CreateHealthCheck
					scope={scope}
					onClose={() => setDialog(null)}
					onSaved={done}
				/>
			)}
			{dialog === "delete" && current && (
				<ActionDialog
					title="Delete health check"
					submit="Delete health check"
					confirmation={current.Id}
					onClose={() => setDialog(null)}
					onSaved={done}
					plan={async () => [
						{
							label: "Delete health check",
							run: () =>
								route53Call(scope, "DeleteHealthCheck", {
									HealthCheckId: current.Id,
								}),
						},
					]}
				>
					<Alert type="warning" header="This cannot be undone">
						Delete the health check <strong>{current.Id}</strong>. A health
						check that a record set still uses cannot be deleted.
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}

function CreateHealthCheck({
	scope,
	onClose,
	onSaved,
}: {
	scope: Route53Scope;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [form, setForm] = useState<HealthCheckForm>(emptyHealthCheckForm);
	const patch = (next: Partial<HealthCheckForm>) =>
		setForm((previous) => ({ ...previous, ...next }));
	const match = form.type.endsWith("_STR_MATCH");
	return (
		<ActionDialog
			title="Create health check"
			submit="Create health check"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const config = buildHealthCheckConfig(form);
				return [
					{
						label: "Create health check",
						run: () =>
							route53Call(scope, "CreateHealthCheck", {
								CallerReference: `console-${Date.now()}-${Math.random().toString(36).slice(2)}`,
								HealthCheckConfig: config,
							}),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<FormField label="Type">
					<Select
						ariaLabel="Health check type"
						selectedOption={{ label: form.type, value: form.type }}
						options={healthCheckTypes.map((value) => ({ label: value, value }))}
						onChange={({ detail }) =>
							patch({ type: detail.selectedOption.value ?? "HTTP" })
						}
					/>
				</FormField>
				<FormField
					label="Domain name"
					description="Enter a domain name, an IP address, or both."
				>
					<Input
						ariaLabel="Domain name"
						value={form.domain}
						onChange={({ detail }) => patch({ domain: detail.value })}
					/>
				</FormField>
				<FormField label="IP address">
					<Input
						ariaLabel="IP address"
						value={form.ipAddress}
						onChange={({ detail }) => patch({ ipAddress: detail.value })}
					/>
				</FormField>
				<FormField
					label="Port"
					description={
						form.type === "TCP"
							? "Required for TCP checks."
							: "Defaults to 80, or 443 for HTTPS."
					}
				>
					<Input
						ariaLabel="Port"
						inputMode="numeric"
						value={form.port}
						onChange={({ detail }) => patch({ port: detail.value })}
					/>
				</FormField>
				{form.type !== "TCP" && (
					<FormField label="Path">
						<Input
							ariaLabel="Path"
							value={form.path}
							onChange={({ detail }) => patch({ path: detail.value })}
						/>
					</FormField>
				)}
				{match && (
					<FormField label="Search string">
						<Input
							ariaLabel="Search string"
							value={form.search}
							onChange={({ detail }) => patch({ search: detail.value })}
						/>
					</FormField>
				)}
				<FormField label="Request interval">
					<Select
						ariaLabel="Request interval"
						selectedOption={{
							label: `${form.interval} seconds`,
							value: form.interval,
						}}
						options={["10", "30"].map((value) => ({
							label: `${value} seconds`,
							value,
						}))}
						onChange={({ detail }) =>
							patch({ interval: detail.selectedOption.value ?? "30" })
						}
					/>
				</FormField>
				<FormField label="Failure threshold" description="1 to 10.">
					<Input
						ariaLabel="Failure threshold"
						inputMode="numeric"
						value={form.threshold}
						onChange={({ detail }) => patch({ threshold: detail.value })}
					/>
				</FormField>
			</SpaceBetween>
		</ActionDialog>
	);
}
