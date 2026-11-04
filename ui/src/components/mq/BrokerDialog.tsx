import {
	Alert,
	Checkbox,
	FormField,
	Input,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Action, Choice, Field } from "#/components/containers/Controls";
import { words } from "#/lib/consoleActions";
import { mqCall, mqList } from "#/lib/mq";
import {
	type Broker,
	type BrokerForm,
	brokerInput,
	type InitialUser,
	type MQConfiguration,
	type MQScope,
	versions,
} from "#/lib/mqHelpers";
import { usePoll } from "#/lib/usePoll";
export function BrokerDialog({
	scope,
	docker,
	broker: b,
	onClose,
	onSaved,
}: {
	scope: MQScope;
	docker: boolean;
	broker?: Broker;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [f, setF] = useState<BrokerForm>({
		name: b?.BrokerName ?? "",
		configuration:
			b?.Configurations?.Pending?.Id ?? b?.Configurations?.Current?.Id ?? "",
		revision: String(
			b?.Configurations?.Pending?.Revision ??
				b?.Configurations?.Current?.Revision ??
				1,
		),
		engine: b?.EngineType ?? "ACTIVEMQ",
		version: b?.EngineVersion ?? "5.18",
		instance: b?.HostInstanceType ?? "mq.m5.large",
		mode: b?.DeploymentMode ?? "SINGLE_INSTANCE",
		public: b?.PubliclyAccessible ?? true,
		subnets: b?.SubnetIds?.join(", ") ?? "",
		groups: b?.SecurityGroups?.join(", ") ?? "",
		general: b?.Logs?.General ?? false,
		audit: b?.Logs?.Audit ?? false,
		day: b?.MaintenanceWindowStartTime?.DayOfWeek ?? "MONDAY",
		time: b?.MaintenanceWindowStartTime?.TimeOfDay ?? "00:00",
		tags: "{}",
	});
	const configurations = usePoll(
		() =>
			mqList<MQConfiguration>(scope, "ListConfigurations", "Configurations"),
		0,
	);
	const [users, setUsers] = useState<(InitialUser & { id: string })[]>([
		{ id: crypto.randomUUID(), Username: "", Password: "" },
	]);
	const set = <K extends keyof BrokerForm>(key: K, value: BrokerForm[K]) =>
		setF({ ...f, [key]: value });
	const user = (i: number, patch: Partial<InitialUser>) =>
		setUsers(users.map((u, j) => (i === j ? { ...u, ...patch } : u)));
	const title = b ? "Edit broker" : "Create broker";
	return (
		<ActionDialog
			title={title}
			submit={title}
			onClose={onClose}
			onSaved={onSaved}
			plan={async (token) => {
				const input = brokerInput(
					f,
					docker,
					users.map(({ id: _id, ...u }) => u),
					b,
				);
				if (!b) input.CreatorRequestId = token;
				return [
					{
						label: b ? "UpdateBroker" : "CreateBroker",
						run: async () => {
							const result = await mqCall(
								scope,
								b ? "UpdateBroker" : "CreateBroker",
								input,
							);
							setUsers([]);
							return result;
						},
					},
				];
			}}
		>
			<SpaceBetween size="m">
				{!b && (
					<SpaceBetween size="m">
						<Field
							label="Broker name"
							value={f.name}
							onChange={(v) => set("name", v)}
						/>
						<Choice
							label="Engine"
							value={f.engine}
							options={["ACTIVEMQ", "RABBITMQ"]}
							onChange={(v) => {
								setF({
									...f,
									engine: v,
									version: v === "ACTIVEMQ" ? "5.18" : "3.13",
									mode: "SINGLE_INSTANCE",
								});
								setUsers([
									{ id: crypto.randomUUID(), Username: "", Password: "" },
								]);
							}}
						/>
					</SpaceBetween>
				)}
				<Choice
					label="Engine version"
					value={f.version}
					options={versions(f.engine, docker)}
					onChange={(v) => set("version", v)}
				/>
				{!docker && (
					<SpaceBetween size="m">
						{!!configurations.error && (
							<Alert type="error">{configurations.error}</Alert>
						)}
						<Choice
							label={
								b
									? "Configuration ID (empty retains current)"
									: "Configuration ID (optional)"
							}
							value={f.configuration ?? ""}
							options={[
								"",
								...(configurations.data ?? [])
									.filter(
										(c) =>
											c.EngineType === f.engine &&
											c.EngineVersion === f.version,
									)
									.map((c) => c.Id),
							]}
							onChange={(v) => set("configuration", v)}
						/>
						{!!f.configuration && (
							<Field
								label="Configuration revision"
								value={f.revision ?? "1"}
								onChange={(v) => set("revision", v)}
							/>
						)}
					</SpaceBetween>
				)}
				<Choice
					label="Instance type"
					value={f.instance}
					options={
						f.engine === "RABBITMQ"
							? ["mq.m5.large"]
							: ["mq.t3.micro", "mq.m5.large"]
					}
					onChange={(v) => set("instance", v)}
				/>
				{!b && (
					<SpaceBetween size="m">
						<Choice
							label="Deployment mode"
							value={f.mode}
							options={[
								"SINGLE_INSTANCE",
								f.engine === "ACTIVEMQ"
									? "ACTIVE_STANDBY_MULTI_AZ"
									: "CLUSTER_MULTI_AZ",
							]}
							onChange={(v) => set("mode", v)}
						/>
						<Checkbox
							checked={f.public}
							onChange={({ detail }) => set("public", detail.checked)}
						>
							Publicly accessible
						</Checkbox>
						{!f.public && (
							<Field
								label="Subnet IDs (1 single, 2 standby, 3 cluster; same VPC, distinct AZs)"
								value={f.subnets}
								onChange={(v) => set("subnets", v)}
							/>
						)}
					</SpaceBetween>
				)}
				{!f.public && (
					<Field
						label="Security group IDs (1–5 from the subnet VPC)"
						value={f.groups}
						onChange={(v) => set("groups", v)}
					/>
				)}
				<Checkbox
					checked={f.general}
					onChange={({ detail }) => set("general", detail.checked)}
				>
					General logs
				</Checkbox>
				{f.engine === "ACTIVEMQ" && (
					<Checkbox
						checked={f.audit}
						onChange={({ detail }) => set("audit", detail.checked)}
					>
						Audit logs
					</Checkbox>
				)}
				<Choice
					label="Maintenance day (UTC)"
					value={f.day}
					options={[
						"MONDAY",
						"TUESDAY",
						"WEDNESDAY",
						"THURSDAY",
						"FRIDAY",
						"SATURDAY",
						"SUNDAY",
					]}
					onChange={(v) => set("day", v)}
				/>
				<Field
					label="Maintenance time (HH:MM UTC)"
					value={f.time}
					onChange={(v) => set("time", v)}
				/>
				<Alert>
					Automatic minor version upgrades and AWS-owned encryption keys are
					required. In Docker mode an engine version update is pending until
					reboot; compatible forward ActiveMQ upgrades are supported, while
					RabbitMQ major/minor upgrades require recreation.
				</Alert>
				{!b && (
					<SpaceBetween size="m">
						<Field
							label="Tags (JSON string map)"
							value={f.tags}
							onChange={(v) => set("tags", v)}
							multiline
						/>
						<Alert>
							Initial credentials cannot be retrieved later. User changes
							require reboot; Docker user mutations and custom configurations
							are unsupported. Private networking references must already exist
							in this account and Region.
						</Alert>
						{users.map((u, i) => (
							<SpaceBetween key={u.id} size="s">
								<Field
									label={`User ${i + 1} username`}
									value={u.Username}
									onChange={(v) => user(i, { Username: v })}
								/>
								<FormField label="Password">
									<Input
										type="password"
										value={u.Password}
										onChange={({ detail }) =>
											user(i, { Password: detail.value })
										}
									/>
								</FormField>
								{f.engine === "ACTIVEMQ" && (
									<SpaceBetween size="s">
										<Checkbox
											checked={u.ConsoleAccess ?? false}
											onChange={({ detail }) =>
												user(i, { ConsoleAccess: detail.checked })
											}
										>
											Web console access
										</Checkbox>
										<Field
											label="Groups"
											value={u.Groups?.join(", ") ?? ""}
											onChange={(v) => user(i, { Groups: words(v) })}
										/>
									</SpaceBetween>
								)}
								{users.length > 1 && (
									<Action
										onClick={() => setUsers(users.filter((_, j) => i !== j))}
									>
										Remove user
									</Action>
								)}
							</SpaceBetween>
						))}
						{f.engine === "ACTIVEMQ" && (
							<Action
								disabled={users.length >= 250}
								onClick={() =>
									setUsers([
										...users,
										{ id: crypto.randomUUID(), Username: "", Password: "" },
									])
								}
							>
								Add user
							</Action>
						)}
					</SpaceBetween>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}
