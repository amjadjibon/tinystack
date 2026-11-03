import { integer, jsonObject, stringMap, words } from "./consoleActions.ts";
import { jsonArray } from "./costValidation.ts";

export type BatchScope = { account: string; region: string };
export type BatchSection = "environments" | "queues" | "definitions" | "jobs";
export const batchSections: BatchSection[] = [
	"environments",
	"queues",
	"definitions",
	"jobs",
];
export const sectionTitles: Record<BatchSection, string> = {
	environments: "Compute environments",
	queues: "Job queues",
	definitions: "Job definitions",
	jobs: "Jobs",
};
type Tags = Record<string, string>;
export type ComputeResources = {
	Type?: string;
	AllocationStrategy?: string;
	MinvCpus?: number;
	MaxvCpus?: number;
	DesiredvCpus?: number;
	InstanceTypes?: string[];
	Subnets?: string[];
	SecurityGroupIds?: string[];
	InstanceRole?: string;
};
export type ComputeEnvironment = {
	ComputeEnvironmentName: string;
	ComputeEnvironmentArn: string;
	Type?: string;
	State?: string;
	Status?: string;
	StatusReason?: string;
	ComputeResources?: ComputeResources;
	ServiceRole?: string;
	Tags?: Tags;
};
export type EnvironmentOrder = { Order?: number; ComputeEnvironment?: string };
export type JobQueue = {
	JobQueueName: string;
	JobQueueArn: string;
	State?: string;
	Status?: string;
	StatusReason?: string;
	Priority?: number;
	ComputeEnvironmentOrder?: EnvironmentOrder[];
	Tags?: Tags;
};
type Pair = { Name?: string; Value?: string };
type Requirement = { Type?: string; Value?: string };
export type ContainerProperties = {
	Image?: string;
	Vcpus?: number;
	Memory?: number;
	Command?: string[];
	JobRoleArn?: string;
	ExecutionRoleArn?: string;
	Environment?: Pair[];
	ResourceRequirements?: Requirement[];
	LogConfiguration?: { LogDriver?: string; Options?: Tags };
};
export type RetryStrategy = {
	Attempts?: number;
	EvaluateOnExit?: Record<string, string>[];
};
type Timeout = { AttemptDurationSeconds?: number };
export type JobDefinition = {
	JobDefinitionName: string;
	JobDefinitionArn: string;
	Revision?: number;
	Status?: string;
	Type?: string;
	Parameters?: Tags;
	ContainerProperties?: ContainerProperties;
	Tags?: Tags;
	PlatformCapabilities?: string[];
	RetryStrategy?: RetryStrategy;
	Timeout?: Timeout;
};
export type JobSummary = {
	JobArn?: string;
	JobId: string;
	JobName?: string;
	CreatedAt?: number;
	Status?: string;
	StatusReason?: string;
	StartedAt?: number;
	StoppedAt?: number;
	JobDefinition?: string;
	Container?: { ExitCode?: number; Reason?: string };
	ArrayProperties?: { Size?: number; Index?: number };
};
export type Attempt = {
	Container?: {
		TaskArn?: string;
		ExitCode?: number;
		Reason?: string;
		LogStreamName?: string;
	};
	StartedAt?: number;
	StoppedAt?: number;
	StatusReason?: string;
};
export type Job = Omit<JobSummary, "Container" | "ArrayProperties"> & {
	JobQueue?: string;
	Attempts?: Attempt[];
	RetryStrategy?: RetryStrategy;
	Timeout?: Timeout;
	DependsOn?: { JobId?: string; Type?: string }[];
	Parameters?: Tags;
	Container?: ContainerProperties & {
		ExitCode?: number;
		Reason?: string;
		TaskArn?: string;
		LogStreamName?: string;
	};
	ArrayProperties?: {
		Size?: number;
		Index?: number;
		StatusSummary?: Record<string, number>;
	};
	Tags?: Tags;
	PlatformCapabilities?: string[];
};

// Batch counts are 32-bit integers.
const maxCount = 2147483647;
export const states = ["ENABLED", "DISABLED"];
export const environmentTypes = ["MANAGED", "UNMANAGED"];
export const strategies = ["BEST_FIT", "BEST_FIT_PROGRESSIVE"];
export const jobStatuses = [
	"SUBMITTED",
	"PENDING",
	"RUNNABLE",
	"STARTING",
	"RUNNING",
	"SUCCEEDED",
	"FAILED",
];
// The status filter that lists every status, of jobs or of job definitions.
export const allStatuses = "ALL";
export const definitionStatuses = ["ACTIVE", "INACTIVE", allStatuses];
const inFlight = ["SUBMITTED", "PENDING", "RUNNABLE", "STARTING", "RUNNING"];
// An environment or queue is changing while it is created, updated or deleted.
export const pending = (status?: string) =>
	["CREATING", "UPDATING", "DELETING"].includes(status ?? "");
// A job changes on its own only when the Docker runtime schedules it. Records
// mode keeps a submitted job RUNNABLE (PENDING behind a dependency) until it is
// cancelled or terminated, which the page reloads for.
export const jobPending = (status: string | undefined, docker: boolean) =>
	docker && inFlight.includes(status ?? "");
export const cancellable = (status?: string) =>
	["SUBMITTED", "PENDING", "RUNNABLE"].includes(status ?? "");
export const terminable = (status?: string) => inFlight.includes(status ?? "");

// statusType maps a state, status or job status to a status indicator.
export function statusType(status?: string) {
	switch (status) {
		case "ENABLED":
		case "VALID":
		case "ACTIVE":
		case "SUCCEEDED":
			return "success" as const;
		case "INVALID":
		case "FAILED":
			return "error" as const;
		case "CREATING":
		case "UPDATING":
		case "DELETING":
		case "STARTING":
		case "RUNNING":
			return "in-progress" as const;
		case "SUBMITTED":
		case "PENDING":
		case "RUNNABLE":
			return "pending" as const;
		default:
			return "stopped" as const;
	}
}

// batchHref links to a section of the page; extra names the resource or filter in it.
export function batchHref(
	s: BatchScope,
	section: BatchSection = "environments",
	extra: Record<string, string | undefined> = {},
) {
	const params = new URLSearchParams({
		account: s.account,
		region: s.region,
		section,
	});
	for (const [key, value] of Object.entries(extra))
		if (value) params.set(key, value);
	return `/services/batch?${params}`;
}

// optionalMap includes a map such as the tags only when it has entries, since
// Batch rejects an empty tag map.
const optionalMap = <K extends string>(
	key: K,
	value: Record<string, string>,
): Partial<Record<K, Record<string, string>>> =>
	Object.keys(value).length
		? ({ [key]: value } as Partial<Record<K, Record<string, string>>>)
		: {};

export function batchName(value: string, label: string) {
	const name = value.trim();
	if (!/^[A-Za-z0-9_-]{1,128}$/.test(name))
		throw new Error(
			`${label} requires 1-128 letters, digits, hyphens or underscores.`,
		);
	return name;
}

// countText shows a number, including 0 such as an exit code, and a dash when absent.
export const countText = (n?: number) => (n === undefined ? "-" : String(n));
// arnName is the name at the end of a Batch ARN, or the text itself.
export const arnName = (arn?: string) => arn?.split("/").pop() || "-";
export const definitionKey = (d: JobDefinition) =>
	`${d.JobDefinitionName}:${d.Revision}`;

// sortDefinitions lists definitions by name with the newest revision first.
export const sortDefinitions = (list: JobDefinition[]) =>
	[...list].sort(
		(a, b) =>
			a.JobDefinitionName.localeCompare(b.JobDefinitionName) ||
			(b.Revision ?? 0) - (a.Revision ?? 0),
	);
// sortJobs lists the newest job first.
export const sortJobs = <T extends { CreatedAt?: number }>(list: T[]) =>
	[...list].sort((a, b) => (b.CreatedAt ?? 0) - (a.CreatedAt ?? 0));

// capacityText shows an environment's minimum, desired and maximum vCPUs.
export const capacityText = (r?: ComputeResources) =>
	r ? `${r.MinvCpus ?? 0} / ${r.DesiredvCpus ?? 0} / ${r.MaxvCpus ?? 0}` : "-";

// environmentNames lists the names of a queue's environments in order of preference.
export const environmentNames = (order?: EnvironmentOrder[]) =>
	[...(order ?? [])]
		.sort((a, b) => (a.Order ?? 0) - (b.Order ?? 0))
		.map((o) => arnName(o.ComputeEnvironment));

// resources is the vCPUs and memory (MiB) a container asks for, which a
// definition gives as VCPU and MEMORY requirements or as the older vcpus and memory.
export function resources(
	c?: Pick<ContainerProperties, "Vcpus" | "Memory" | "ResourceRequirements">,
) {
	const requirement = (type: string) => {
		const value = c?.ResourceRequirements?.find((r) => r.Type === type)?.Value;
		return value ? Number(value) : undefined;
	};
	return {
		vcpus: requirement("VCPU") ?? c?.Vcpus,
		memory: requirement("MEMORY") ?? c?.Memory,
	};
}

// jobLogGroup is the log group the job's container writes to.
export const jobLogGroup = (job: Job) =>
	job.Container?.LogConfiguration?.Options?.["awslogs-group"] ||
	"/aws/batch/job";

// stringList reads a JSON array of strings, such as a command.
export function stringList(text: string, label: string) {
	let value: unknown;
	try {
		value = JSON.parse(text);
	} catch {
		throw new Error(`${label} must be valid JSON.`);
	}
	if (
		!Array.isArray(value) ||
		value.length > 100 ||
		value.some((v) => typeof v !== "string")
	)
		throw new Error(`${label} must be a JSON array of up to 100 strings.`);
	return value as string[];
}

export type EnvironmentForm = {
	name: string;
	type: string;
	serviceRole: string;
	strategy: string;
	min: string;
	desired: string;
	max: string;
	instances: string;
	instanceRole: string;
	subnets: string[];
	groups: string[];
	tags: string;
};
export const emptyEnvironmentForm = (): EnvironmentForm => ({
	name: "",
	type: "MANAGED",
	serviceRole: "",
	strategy: "BEST_FIT_PROGRESSIVE",
	min: "0",
	desired: "0",
	max: "16",
	instances: "optimal",
	instanceRole: "ecsInstanceRole",
	subnets: [],
	groups: [],
	tags: "{}",
});
const instanceType =
	/^(?:(?:c5|m5|r5)(?:\.(?:large|xlarge|2xlarge|4xlarge|8xlarge|12xlarge|16xlarge|24xlarge|metal))?|optimal|default_x86_64)$/;

function capacity(f: { min: string; desired: string; max: string }) {
	const MinvCpus = integer(f.min.trim(), "Minimum vCPUs", 0, maxCount),
		DesiredvCpus = integer(f.desired.trim(), "Desired vCPUs", 0, maxCount),
		MaxvCpus = integer(f.max.trim(), "Maximum vCPUs", 1, maxCount);
	if (MinvCpus > DesiredvCpus || DesiredvCpus > MaxvCpus)
		throw new Error(
			"Minimum vCPUs cannot exceed desired vCPUs, which cannot exceed maximum vCPUs.",
		);
	return { MinvCpus, DesiredvCpus, MaxvCpus };
}

// environmentInput builds CreateComputeEnvironment. The backend creates both
// types ENABLED and supports EC2 capacity only.
export function environmentInput(f: EnvironmentForm) {
	const ComputeEnvironmentName = batchName(f.name, "Name");
	if (!environmentTypes.includes(f.type))
		throw new Error("Choose a managed or unmanaged environment.");
	const Tags = stringMap(f.tags, "Tags"),
		role = f.serviceRole.trim();
	const input: Record<string, unknown> = {
		ComputeEnvironmentName,
		Type: f.type,
		State: "ENABLED",
		...optionalMap("Tags", Tags),
		...(role ? { ServiceRole: role } : {}),
	};
	if (f.type === "UNMANAGED") {
		if (!role)
			throw new Error("Unmanaged environments require a service role.");
		return input;
	}
	if (!strategies.includes(f.strategy))
		throw new Error("Choose an allocation strategy.");
	const InstanceTypes = words(f.instances);
	if (
		!InstanceTypes.length ||
		InstanceTypes.length > 100 ||
		new Set(InstanceTypes).size !== InstanceTypes.length ||
		!InstanceTypes.every((t) => instanceType.test(t))
	)
		throw new Error(
			"Enter distinct instance types: c5, m5 or r5 (with an optional size), optimal or default_x86_64.",
		);
	const InstanceRole = f.instanceRole.trim();
	if (!InstanceRole) throw new Error("Enter an instance profile.");
	if (f.subnets.length < 1 || f.subnets.length > 16)
		throw new Error("Choose 1-16 subnets.");
	if (f.groups.length < 1 || f.groups.length > 5)
		throw new Error("Choose 1-5 security groups.");
	input.ComputeResources = {
		Type: "EC2",
		AllocationStrategy: f.strategy,
		...capacity(f),
		InstanceTypes,
		InstanceRole,
		Subnets: f.subnets,
		SecurityGroupIds: f.groups,
	};
	return input;
}

// environmentUpdate builds UpdateComputeEnvironment with only what changed.
export function environmentUpdate(
	c: ComputeEnvironment,
	f: { state: string; min: string; desired: string; max: string },
) {
	if (!states.includes(f.state)) throw new Error("Choose a state.");
	const input: Record<string, unknown> = {
		ComputeEnvironment: c.ComputeEnvironmentName,
	};
	if (f.state !== c.State) input.State = f.state;
	const r = c.ComputeResources;
	if (r) {
		const next = capacity(f);
		if (next.DesiredvCpus < (r.DesiredvCpus ?? 0))
			throw new Error("Desired vCPUs cannot decrease.");
		const changes = {
			...(next.MinvCpus !== r.MinvCpus ? { MinvCpus: next.MinvCpus } : {}),
			...(next.DesiredvCpus !== r.DesiredvCpus
				? { DesiredvCpus: next.DesiredvCpus }
				: {}),
			...(next.MaxvCpus !== r.MaxvCpus ? { MaxvCpus: next.MaxvCpus } : {}),
		};
		if (Object.keys(changes).length) input.ComputeResources = changes;
	}
	if (Object.keys(input).length === 1)
		throw new Error("Change at least one setting.");
	return input;
}

export type QueueForm = {
	name: string;
	state: string;
	priority: string;
	// Names of the environments in order of preference.
	environments: string[];
	tags: string;
};
export const emptyQueueForm = (): QueueForm => ({
	name: "",
	state: "ENABLED",
	priority: "1",
	environments: [""],
	tags: "{}",
});

function order(environments: string[], required: boolean) {
	const names = environments.filter(Boolean);
	if (names.length !== environments.length)
		throw new Error("Choose an environment for every row or remove the row.");
	if (names.length > 3 || (required && !names.length))
		throw new Error(
			required
				? "Choose 1-3 compute environments."
				: "Choose at most 3 compute environments.",
		);
	if (new Set(names).size !== names.length)
		throw new Error("Choose each compute environment once.");
	return names.map((ComputeEnvironment, i) => ({
		Order: i + 1,
		ComputeEnvironment,
	}));
}

export function queueInput(f: QueueForm) {
	if (!states.includes(f.state)) throw new Error("Choose a state.");
	return {
		JobQueueName: batchName(f.name, "Name"),
		State: f.state,
		Priority: integer(f.priority.trim(), "Priority", 0, maxCount),
		ComputeEnvironmentOrder: order(f.environments, true),
		...optionalMap("Tags", stringMap(f.tags, "Tags")),
	};
}

// queueUpdate builds UpdateJobQueue with only what changed. An empty
// environment list takes the queue off all of its environments.
export function queueUpdate(
	q: JobQueue,
	f: { state: string; priority: string; environments: string[] },
) {
	if (!states.includes(f.state)) throw new Error("Choose a state.");
	const input: Record<string, unknown> = { JobQueue: q.JobQueueName };
	if (f.state !== q.State) input.State = f.state;
	const Priority = integer(f.priority.trim(), "Priority", 0, maxCount);
	if (Priority !== q.Priority) input.Priority = Priority;
	const ComputeEnvironmentOrder = order(f.environments, false);
	if (
		ComputeEnvironmentOrder.map((o) => o.ComputeEnvironment).join("\n") !==
		environmentNames(q.ComputeEnvironmentOrder).join("\n")
	)
		input.ComputeEnvironmentOrder = ComputeEnvironmentOrder;
	if (Object.keys(input).length === 1)
		throw new Error("Change at least one setting.");
	return input;
}

export type DefinitionForm = {
	name: string;
	container: string;
	parameters: string;
	attempts: string;
	rules: string;
	timeout: string;
	tags: string;
};
// Console actions take member names in any case, so the JSON may use the
// lower-case names of the AWS CLI or the names shown here.
const containerTemplate: ContainerProperties = {
	Image: "busybox",
	Command: ["echo", "hello"],
	ResourceRequirements: [
		{ Type: "VCPU", Value: "1" },
		{ Type: "MEMORY", Value: "512" },
	],
};
// definitionForm starts a form from a definition, which registers its next revision.
export function definitionForm(from?: JobDefinition): DefinitionForm {
	return {
		name: from?.JobDefinitionName ?? "",
		container: JSON.stringify(
			from?.ContainerProperties ?? containerTemplate,
			null,
			2,
		),
		parameters: JSON.stringify(from?.Parameters ?? {}),
		attempts: from?.RetryStrategy?.Attempts
			? String(from.RetryStrategy.Attempts)
			: "",
		rules: from?.RetryStrategy?.EvaluateOnExit?.length
			? JSON.stringify(from.RetryStrategy.EvaluateOnExit, null, 2)
			: "",
		timeout: from?.Timeout?.AttemptDurationSeconds
			? String(from.Timeout.AttemptDurationSeconds)
			: "",
		tags: JSON.stringify(from?.Tags ?? {}),
	};
}

// retryStrategy reads the attempts and the optional evaluateOnExit rules.
export function retryStrategy(attempts: string, rules: string) {
	if (!attempts.trim()) {
		if (rules.trim()) throw new Error("Retry rules need a number of attempts.");
		return undefined;
	}
	return {
		Attempts: integer(attempts.trim(), "Retry attempts", 1, 10),
		...(rules.trim()
			? { EvaluateOnExit: jsonArray(rules, "Retry rules", 1, 6) }
			: {}),
	};
}

// timeout reads a job attempt time limit; Batch allows at least 60 seconds.
export function timeout(text: string) {
	return text.trim()
		? {
				AttemptDurationSeconds: integer(
					text.trim(),
					"Timeout (seconds)",
					60,
					maxCount,
				),
			}
		: undefined;
}

// definitionInput builds RegisterJobDefinition. The backend registers
// container definitions on EC2 only.
export function definitionInput(f: DefinitionForm) {
	const RetryStrategy = retryStrategy(f.attempts, f.rules),
		Timeout = timeout(f.timeout);
	return {
		JobDefinitionName: batchName(f.name, "Name"),
		Type: "container",
		PlatformCapabilities: ["EC2"],
		ContainerProperties: jsonObject(f.container, "Container properties"),
		...optionalMap("Parameters", stringMap(f.parameters, "Parameters")),
		...optionalMap("Tags", stringMap(f.tags, "Tags")),
		...(RetryStrategy ? { RetryStrategy } : {}),
		...(Timeout ? { Timeout } : {}),
	};
}

export type SubmitForm = {
	name: string;
	queue: string;
	// name:revision of an active definition.
	definition: string;
	parameters: string;
	command: string;
	environment: string;
	vcpus: string;
	memory: string;
	attempts: string;
	timeout: string;
	size: string;
	depends: string;
	sequential: boolean;
	tags: string;
};
export const emptySubmitForm = (queue = "", definition = ""): SubmitForm => ({
	name: "",
	queue,
	definition,
	parameters: "{}",
	command: "",
	environment: "{}",
	vcpus: "",
	memory: "",
	attempts: "",
	timeout: "",
	size: "",
	depends: "",
	sequential: false,
	tags: "{}",
});

// containerOverrides builds the overrides a job asks for, or nothing. Batch
// replaces the vCPU and memory requirements together, so a value left blank
// keeps what the definition sets.
function containerOverrides(f: SubmitForm, definition: JobDefinition) {
	const overrides: Record<string, unknown> = {};
	if (f.command.trim())
		overrides.Command = stringList(f.command.trim(), "Command");
	const environment = stringMap(f.environment, "Environment");
	const names = Object.keys(environment);
	if (names.some((n) => !/^[A-Za-z_]\w*$/.test(n) || n.startsWith("AWS_BATCH")))
		throw new Error(
			"Environment names use letters, digits and underscores and do not start with AWS_BATCH.",
		);
	if (names.length)
		overrides.Environment = names.map((Name) => ({
			Name,
			Value: environment[Name],
		}));
	if (f.vcpus.trim() || f.memory.trim()) {
		const current = resources(definition.ContainerProperties);
		const vcpus = f.vcpus.trim()
				? integer(f.vcpus.trim(), "vCPUs", 1, maxCount)
				: current.vcpus,
			memory = f.memory.trim()
				? integer(f.memory.trim(), "Memory (MiB)", 4, maxCount)
				: current.memory;
		if (vcpus === undefined || memory === undefined)
			throw new Error("Enter both vCPUs and memory.");
		overrides.ResourceRequirements = [
			{ Type: "VCPU", Value: String(vcpus) },
			{ Type: "MEMORY", Value: String(memory) },
		];
	}
	return Object.keys(overrides).length ? overrides : undefined;
}

// submitInput builds SubmitJob from the form and the active definitions.
export function submitInput(f: SubmitForm, definitions: JobDefinition[]) {
	const JobName = batchName(f.name, "Job name");
	if (!f.queue) throw new Error("Choose a job queue.");
	const definition = definitions.find((d) => definitionKey(d) === f.definition);
	if (!definition) throw new Error("Choose a job definition.");
	const ContainerOverrides = containerOverrides(f, definition);
	const attempts = f.attempts.trim(),
		Timeout = timeout(f.timeout);
	const size = f.size.trim();
	if (f.sequential && !size)
		throw new Error("Sequential children need an array size.");
	const dependencies = [
		...words(f.depends).map((JobId) => ({ JobId })),
		...(f.sequential ? [{ Type: "SEQUENTIAL" }] : []),
	];
	if (dependencies.length > 20) throw new Error("Depend on at most 20 jobs.");
	const Parameters = stringMap(f.parameters, "Parameters"),
		Tags = stringMap(f.tags, "Tags");
	return {
		JobName,
		JobQueue: f.queue,
		JobDefinition: definitionKey(definition),
		...optionalMap("Parameters", Parameters),
		...(ContainerOverrides ? { ContainerOverrides } : {}),
		...(attempts
			? {
					RetryStrategy: {
						Attempts: integer(attempts, "Retry attempts", 1, 10),
					},
				}
			: {}),
		...(Timeout ? { Timeout } : {}),
		...(size
			? { ArrayProperties: { Size: integer(size, "Array size", 2, 10000) } }
			: {}),
		...(dependencies.length ? { DependsOn: dependencies } : {}),
		...optionalMap("Tags", Tags),
	};
}
