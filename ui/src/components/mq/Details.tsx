import {
	Alert,
	CopyToClipboard,
	Link,
	SpaceBetween,
} from "@cloudscape-design/components";
import { Rows } from "#/components/containers/Controls";
import { isoTime } from "#/lib/format";
import type { Broker } from "#/lib/mqHelpers";

const copy = (value: string) => (
	<CopyToClipboard
		variant="inline"
		textToCopy={value}
		copySuccessText="Copied"
		copyErrorText="Copy failed"
	/>
);
export function Details({
	broker: b,
	docker,
}: {
	broker: Broker;
	docker: boolean;
}) {
	return (
		<SpaceBetween size="m">
			<div>ARN: {copy(b.BrokerArn)}</div>
			<div>ID: {copy(b.BrokerId)}</div>
			<div>
				State: {b.BrokerState ?? "-"} · Created: {isoTime(b.Created)}
			</div>
			<div>
				{b.EngineType} {b.EngineVersion} · {b.HostInstanceType} ·{" "}
				{b.DeploymentMode}
			</div>
			{!!b.PendingEngineVersion && (
				<Alert>
					Pending engine version: {b.PendingEngineVersion}. Reboot to apply.
				</Alert>
			)}
			<div>
				Publicly accessible: {String(b.PubliclyAccessible)} · Automatic minor
				upgrades: {String(b.AutoMinorVersionUpgrade)}
			</div>
			<div>Subnets: {b.SubnetIds?.join(", ") || "-"}</div>
			<div>Security groups: {b.SecurityGroups?.join(", ") || "-"}</div>
			<div>
				General logs: {String(b.Logs?.General ?? false)}
				{b.EngineType === "ACTIVEMQ"
					? ` · Audit logs: ${String(b.Logs?.Audit ?? false)}`
					: ""}
			</div>
			<div>
				Maintenance: {b.MaintenanceWindowStartTime?.DayOfWeek ?? "-"}{" "}
				{b.MaintenanceWindowStartTime?.TimeOfDay ?? "-"} UTC
			</div>
			{docker && b.BrokerState !== "RUNNING" && (
				<Alert>
					Connection details are available once the broker is RUNNING.
				</Alert>
			)}
			{(b.BrokerInstances ?? []).map((instance) => (
				<SpaceBetween
					key={instance.ConsoleURL ?? instance.Endpoints?.join(",")}
					size="s"
				>
					{!!instance.ConsoleURL && (
						<div>
							Web console: {copy(instance.ConsoleURL)}{" "}
							<Link href={instance.ConsoleURL} external>
								Open console
							</Link>
						</div>
					)}
					{(instance.Endpoints ?? []).map((endpoint) => (
						<div key={endpoint}>Endpoint: {copy(endpoint)}</div>
					))}
				</SpaceBetween>
			))}
			{b.EngineType === "ACTIVEMQ" && (
				<Rows
					title="Users"
					items={b.Users ?? []}
					emptyText="Users are supplied when creating a broker."
					columns={[
						{ id: "username", header: "Username", cell: (u) => u.Username },
					]}
				/>
			)}
			<div>
				Current configuration:{" "}
				{b.Configurations?.Current
					? `${b.Configurations.Current.Id} revision ${b.Configurations.Current.Revision}`
					: "-"}
			</div>
			{!!b.Configurations?.Pending && (
				<Alert>
					Pending configuration: {b.Configurations.Pending.Id} revision{" "}
					{b.Configurations.Pending.Revision}. Reboot to apply.
				</Alert>
			)}
			{!!b.Configurations?.History?.length && (
				<div>
					Configuration history:{" "}
					{b.Configurations.History.map(
						(c) => `${c.Id} revision ${c.Revision}`,
					).join(", ")}
				</div>
			)}
		</SpaceBetween>
	);
}
