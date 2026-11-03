import { stringMap, words } from "./consoleActions.ts";
export type MQScope = { account: string; region: string };
export type Broker = {
	BrokerId: string;
	BrokerArn: string;
	BrokerName: string;
	EngineType: string;
	EngineVersion: string;
	DeploymentMode: string;
	HostInstanceType: string;
	BrokerState?: string;
	Created?: string;
	PendingEngineVersion?: string;
	PubliclyAccessible?: boolean;
	AutoMinorVersionUpgrade?: boolean;
	SubnetIds?: string[];
	SecurityGroups?: string[];
	Tags?: Record<string, string>;
	Logs?: { General?: boolean; Audit?: boolean };
	MaintenanceWindowStartTime?: {
		DayOfWeek?: string;
		TimeOfDay?: string;
		TimeZone?: string;
	};
	Users?: { Username: string }[];
	BrokerInstances?: { ConsoleURL?: string; Endpoints?: string[] }[];
};
export const pending = (state?: string) =>
	!!state && state.endsWith("_IN_PROGRESS");
export function mqHref(s: MQScope, broker = "", tab = "overview") {
	const p = new URLSearchParams({ ...s, tab });
	if (broker) p.set("broker", broker);
	return `/services/mq?${p}`;
}
export function versions(engine: string, docker: boolean) {
	return engine === "ACTIVEMQ"
		? docker
			? ["5.17", "5.17.6", "5.18", "5.18.7", "5.19", "5.19.0"]
			: ["5.18", "5.19"]
		: docker
			? ["3.13", "3.13.7", "4.0", "4.0.9", "4.1", "4.1.4"]
			: ["3.13"];
}
export type BrokerForm = {
	name: string;
	engine: string;
	version: string;
	instance: string;
	mode: string;
	public: boolean;
	subnets: string;
	groups: string;
	general: boolean;
	audit: boolean;
	day: string;
	time: string;
	tags: string;
};
export type InitialUser = {
	Username: string;
	Password: string;
	ConsoleAccess?: boolean;
	Groups?: string[];
};
export function brokerInput(
	f: BrokerForm,
	docker: boolean,
	users: InitialUser[],
	broker?: Broker,
): Record<string, unknown> {
	if (!versions(f.engine, docker).includes(f.version))
		throw new Error("Choose a supported engine version.");
	if (!["mq.t3.micro", "mq.m5.large"].includes(f.instance))
		throw new Error("Choose a supported instance type.");
	if (
		f.instance === "mq.t3.micro" &&
		(f.mode !== "SINGLE_INSTANCE" || (!broker && f.engine === "RABBITMQ"))
	)
		throw new Error(
			"Micro instances require a single ActiveMQ broker for creation.",
		);
	if (
		![
			"SINGLE_INSTANCE",
			f.engine === "ACTIVEMQ" ? "ACTIVE_STANDBY_MULTI_AZ" : "CLUSTER_MULTI_AZ",
		].includes(f.mode)
	)
		throw new Error("Choose a deployment mode supported by the engine.");
	if (!/^(?:[01]\d|2[0-3]):[0-5]\d$/.test(f.time))
		throw new Error("Maintenance time requires HH:MM in UTC.");
	if (!f.public) {
		const groups = words(f.groups),
			subnets = words(f.subnets);
		if (
			groups.length < 1 ||
			groups.length > 5 ||
			new Set(groups).size !== groups.length
		)
			throw new Error("Provide one to five distinct security groups.");
		const count =
			f.mode === "SINGLE_INSTANCE" ? 1 : f.mode === "CLUSTER_MULTI_AZ" ? 3 : 2;
		if (
			!broker &&
			(subnets.length !== count || new Set(subnets).size !== count)
		)
			throw new Error(
				`Provide ${count} distinct subnets for this deployment mode.`,
			);
	}
	const input: Record<string, unknown> = {
		EngineVersion: f.version,
		HostInstanceType: f.instance,
		AutoMinorVersionUpgrade: true,
		Logs: {
			General: f.general,
			...(f.engine === "ACTIVEMQ" ? { Audit: f.audit } : {}),
		},
		MaintenanceWindowStartTime: {
			DayOfWeek: f.day,
			TimeOfDay: f.time,
			TimeZone: "UTC",
		},
		...(!f.public ? { SecurityGroups: words(f.groups) } : {}),
	};
	if (broker) return { ...input, BrokerId: broker.BrokerId };
	if (!/^[A-Za-z0-9_-]{1,50}$/.test(f.name.trim()))
		throw new Error(
			"Name requires 1–50 letters, digits, hyphens or underscores.",
		);
	if (
		!users.length ||
		users.length > 250 ||
		(f.engine === "RABBITMQ" && users.length !== 1)
	)
		throw new Error(
			"Provide 1–250 ActiveMQ users or exactly one RabbitMQ administrator.",
		);
	const seen = new Set<string>();
	for (const u of users) {
		if (
			!/^[A-Za-z0-9_.~-]{2,100}$/.test(u.Username) ||
			seen.has(u.Username) ||
			(f.engine === "RABBITMQ" &&
				(u.Username === "guest" || u.Username.includes("~")))
		)
			throw new Error("Invalid or duplicate username.");
		seen.add(u.Username);
		if (
			!/^[\x20-\x7e]{12,250}$/.test(u.Password) ||
			/[,:=]/.test(u.Password) ||
			new Set(u.Password).size < 4
		)
			throw new Error(
				"Passwords require 12–250 printable ASCII characters, four unique characters, and no comma, colon or equals.",
			);
	}
	return {
		...input,
		BrokerName: f.name.trim(),
		EngineType: f.engine,
		DeploymentMode: f.mode,
		PubliclyAccessible: f.public,
		...(!f.public ? { SubnetIds: words(f.subnets) } : {}),
		EncryptionOptions: { UseAwsOwnedKey: true },
		Tags: stringMap(f.tags, "Tags"),
		Users: users,
	};
}
