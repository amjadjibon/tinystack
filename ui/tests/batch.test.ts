import assert from "node:assert/strict";
import test from "node:test";
import {
	arnName,
	batchHref,
	batchName,
	type ComputeEnvironment,
	cancellable,
	definitionForm,
	definitionInput,
	definitionKey,
	type EnvironmentForm,
	emptyEnvironmentForm,
	emptyQueueForm,
	emptySubmitForm,
	environmentInput,
	environmentNames,
	environmentUpdate,
	type Job,
	type JobDefinition,
	type JobQueue,
	jobLogGroup,
	jobPending,
	pending,
	queueInput,
	queueUpdate,
	resources,
	retryStrategy,
	sortDefinitions,
	sortJobs,
	statusType,
	stringList,
	submitInput,
	terminable,
	timeout,
} from "../src/lib/batchHelpers.ts";
import { words } from "../src/lib/consoleActions.ts";
import { unixMillis } from "../src/lib/format.ts";

const arn = (kind: string, name: string) =>
	`arn:aws:batch:us-east-1:000000000000:${kind}/${name}`;
const environment: ComputeEnvironment = {
	ComputeEnvironmentName: "ce",
	ComputeEnvironmentArn: arn("compute-environment", "ce"),
	Type: "MANAGED",
	State: "ENABLED",
	Status: "VALID",
	ComputeResources: {
		Type: "EC2",
		MinvCpus: 0,
		DesiredvCpus: 2,
		MaxvCpus: 8,
	},
};
const queue: JobQueue = {
	JobQueueName: "q",
	JobQueueArn: arn("job-queue", "q"),
	State: "ENABLED",
	Status: "VALID",
	Priority: 5,
	ComputeEnvironmentOrder: [
		{ Order: 2, ComputeEnvironment: arn("compute-environment", "b") },
		{ Order: 1, ComputeEnvironment: arn("compute-environment", "a") },
	],
};
const definition: JobDefinition = {
	JobDefinitionName: "worker",
	JobDefinitionArn: arn("job-definition", "worker:3"),
	Revision: 3,
	Status: "ACTIVE",
	Type: "container",
	Parameters: { message: "hi" },
	ContainerProperties: {
		Image: "busybox",
		Command: ["echo", "Ref::message"],
		ResourceRequirements: [
			{ Type: "VCPU", Value: "2" },
			{ Type: "MEMORY", Value: "1024" },
		],
	},
	RetryStrategy: { Attempts: 3, EvaluateOnExit: [{ OnExitCode: "1" }] },
	Timeout: { AttemptDurationSeconds: 90 },
	Tags: { env: "test" },
};
const managed = (patch: Partial<EnvironmentForm> = {}): EnvironmentForm => ({
	...emptyEnvironmentForm(),
	name: "ce",
	min: "0",
	desired: "2",
	max: "8",
	instances: "c5.large, m5",
	subnets: ["subnet-a"],
	groups: ["sg-a"],
	...patch,
});

test("environments and queues change while they are created, updated or deleted", () => {
	for (const status of ["CREATING", "UPDATING", "DELETING"])
		assert.equal(pending(status), true);
	for (const status of ["VALID", "INVALID", undefined])
		assert.equal(pending(status), false);
});

test("jobs change on their own only when a runtime schedules them", () => {
	for (const status of [
		"SUBMITTED",
		"PENDING",
		"RUNNABLE",
		"STARTING",
		"RUNNING",
	]) {
		assert.equal(jobPending(status, true), true);
		assert.equal(jobPending(status, false), false);
	}
	for (const status of ["SUCCEEDED", "FAILED", undefined])
		assert.equal(jobPending(status, true), false);
});

test("a job is cancelled before it starts and terminated until it ends", () => {
	assert.deepEqual(
		["SUBMITTED", "PENDING", "RUNNABLE", "STARTING", "RUNNING"].map(
			cancellable,
		),
		[true, true, true, false, false],
	);
	assert.deepEqual(
		["SUBMITTED", "PENDING", "RUNNABLE", "STARTING", "RUNNING"].map(terminable),
		[true, true, true, true, true],
	);
	assert.equal(terminable("SUCCEEDED"), false);
	assert.equal(terminable("FAILED"), false);
	assert.equal(cancellable("FAILED"), false);
});

test("statuses map to indicators", () => {
	assert.deepEqual(
		[
			"ENABLED",
			"VALID",
			"ACTIVE",
			"SUCCEEDED",
			"FAILED",
			"INVALID",
			"RUNNING",
			"STARTING",
			"CREATING",
			"RUNNABLE",
			"PENDING",
			"SUBMITTED",
			"DISABLED",
			"INACTIVE",
			undefined,
		].map(statusType),
		[
			"success",
			"success",
			"success",
			"success",
			"error",
			"error",
			"in-progress",
			"in-progress",
			"in-progress",
			"pending",
			"pending",
			"pending",
			"stopped",
			"stopped",
			"stopped",
		],
	);
});

test("names follow the Batch pattern", () => {
	assert.equal(batchName(" my_job-1 ", "Name"), "my_job-1");
	for (const bad of ["", "has space", "a.b", "x".repeat(129)])
		assert.throws(() => batchName(bad, "Name"), /Name requires 1-128/);
	assert.equal(batchName("x".repeat(128), "Name").length, 128);
});

test("links carry the scope, section and only the parameters given", () => {
	assert.equal(
		batchHref({ account: "000000000000", region: "us-east-1" }),
		"/services/batch?account=000000000000&region=us-east-1&section=environments",
	);
	assert.equal(
		batchHref({ account: "000000000000", region: "us-east-1" }, "jobs", {
			queue: "q",
			status: "",
			job: undefined,
			tab: "tags",
		}),
		"/services/batch?account=000000000000&region=us-east-1&section=jobs&queue=q&tab=tags",
	);
});

test("a managed environment retains EC2 capacity, networking and tags", () => {
	const out = environmentInput(managed({ tags: '{"env":"test"}' }));
	assert.deepEqual(out, {
		ComputeEnvironmentName: "ce",
		Type: "MANAGED",
		State: "ENABLED",
		Tags: { env: "test" },
		ComputeResources: {
			Type: "EC2",
			AllocationStrategy: "BEST_FIT_PROGRESSIVE",
			MinvCpus: 0,
			DesiredvCpus: 2,
			MaxvCpus: 8,
			InstanceTypes: ["c5.large", "m5"],
			InstanceRole: "ecsInstanceRole",
			Subnets: ["subnet-a"],
			SecurityGroupIds: ["sg-a"],
		},
	});
	assert.equal(
		environmentInput(managed({ serviceRole: " arn:aws:iam::0:role/b " }))
			.ServiceRole,
		"arn:aws:iam::0:role/b",
	);
});

test("a managed environment follows the backend limits", () => {
	for (const patch of [
		{ name: "bad name" },
		{ min: "3" },
		{ desired: "9" },
		{ max: "0", desired: "0" },
		{ max: "1.5" },
		{ instances: "" },
		{ instances: "t3.micro" },
		{ instances: "m5, m5" },
		{ instances: "m5.nano" },
		{ instanceRole: " " },
		{ subnets: [] },
		{ groups: [] },
		{ groups: ["a", "b", "c", "d", "e", "f"] },
		{ strategy: "SPOT_CAPACITY_OPTIMIZED" },
		{ tags: '{"a":1}' },
		{ type: "FARGATE" },
	])
		assert.throws(
			() => environmentInput(managed(patch)),
			Error,
			JSON.stringify(patch),
		);
	for (const type of ["optimal", "default_x86_64", "r5.metal", "c5"])
		assert.deepEqual(
			(
				environmentInput(managed({ instances: type })).ComputeResources as {
					InstanceTypes: string[];
				}
			).InstanceTypes,
			[type],
		);
});

test("an unmanaged environment needs a service role and no capacity", () => {
	const form = {
		...emptyEnvironmentForm(),
		name: "outside",
		type: "UNMANAGED",
	};
	assert.throws(() => environmentInput(form), /service role/);
	assert.deepEqual(environmentInput({ ...form, serviceRole: "batch" }), {
		ComputeEnvironmentName: "outside",
		Type: "UNMANAGED",
		State: "ENABLED",
		ServiceRole: "batch",
	});
});

test("empty tag and parameter maps are left out, since Batch rejects an empty tag map", () => {
	assert.equal("Tags" in environmentInput(managed()), false);
	assert.equal(
		"Tags" in
			queueInput({ ...emptyQueueForm(), name: "q", environments: ["a"] }),
		false,
	);
	const registered = definitionInput({ ...definitionForm(), name: "d" });
	assert.equal("Tags" in registered, false);
	assert.equal("Parameters" in registered, false);
	const submitted = submitInput(
		{ ...emptySubmitForm("q", "worker:3"), name: "j" },
		[definition],
	);
	assert.equal("Tags" in submitted, false);
	assert.equal("Parameters" in submitted, false);
	assert.deepEqual(
		definitionInput({ ...definitionForm(), name: "d", tags: '{"a":"b"}' }).Tags,
		{ a: "b" },
	);
});

test("an environment update sends only the changes", () => {
	const same = { state: "ENABLED", min: "0", desired: "2", max: "8" };
	assert.throws(() => environmentUpdate(environment, same), /at least one/);
	assert.deepEqual(
		environmentUpdate(environment, { ...same, state: "DISABLED" }),
		{ ComputeEnvironment: "ce", State: "DISABLED" },
	);
	assert.deepEqual(
		environmentUpdate(environment, { ...same, desired: "4", max: "16" }),
		{
			ComputeEnvironment: "ce",
			ComputeResources: { DesiredvCpus: 4, MaxvCpus: 16 },
		},
	);
	assert.throws(
		() => environmentUpdate(environment, { ...same, desired: "1" }),
		/cannot decrease/,
	);
	assert.throws(
		() => environmentUpdate(environment, { ...same, min: "5" }),
		/cannot exceed/,
	);
	assert.throws(() => environmentUpdate(environment, { ...same, state: "x" }));
	// An unmanaged environment has no capacity to change.
	assert.deepEqual(
		environmentUpdate(
			{ ...environment, Type: "UNMANAGED", ComputeResources: undefined },
			{ ...same, state: "DISABLED", min: "", desired: "", max: "" },
		),
		{ ComputeEnvironment: "ce", State: "DISABLED" },
	);
});

test("a queue lists its environments in the order chosen", () => {
	const out = queueInput({
		...emptyQueueForm(),
		name: "q",
		priority: "7",
		environments: ["b", "a"],
		tags: '{"env":"test"}',
	});
	assert.deepEqual(out, {
		JobQueueName: "q",
		State: "ENABLED",
		Priority: 7,
		ComputeEnvironmentOrder: [
			{ Order: 1, ComputeEnvironment: "b" },
			{ Order: 2, ComputeEnvironment: "a" },
		],
		Tags: { env: "test" },
	});
	for (const patch of [
		{ name: "" },
		{ priority: "-1" },
		{ priority: "high" },
		{ environments: [] },
		{ environments: [""] },
		{ environments: ["a", "a"] },
		{ environments: ["a", "b", "c", "d"] },
		{ state: "PAUSED" },
	])
		assert.throws(
			() =>
				queueInput({
					...emptyQueueForm(),
					name: "q",
					environments: ["a"],
					...patch,
				}),
			Error,
			JSON.stringify(patch),
		);
});

test("a queue update sends only the changes", () => {
	assert.deepEqual(environmentNames(queue.ComputeEnvironmentOrder), ["a", "b"]);
	const same = { state: "ENABLED", priority: "5", environments: ["a", "b"] };
	// The order numbers are not compared, only the sequence of environments.
	assert.throws(() => queueUpdate(queue, same), /at least one/);
	assert.deepEqual(queueUpdate(queue, { ...same, priority: "9" }), {
		JobQueue: "q",
		Priority: 9,
	});
	assert.deepEqual(queueUpdate(queue, { ...same, environments: ["b", "a"] }), {
		JobQueue: "q",
		ComputeEnvironmentOrder: [
			{ Order: 1, ComputeEnvironment: "b" },
			{ Order: 2, ComputeEnvironment: "a" },
		],
	});
	assert.deepEqual(queueUpdate(queue, { ...same, environments: [] }), {
		JobQueue: "q",
		ComputeEnvironmentOrder: [],
	});
	assert.deepEqual(queueUpdate(queue, { ...same, state: "DISABLED" }), {
		JobQueue: "q",
		State: "DISABLED",
	});
	assert.throws(() =>
		queueUpdate(queue, { ...same, environments: ["a", "a"] }),
	);
});

test("retry rules need attempts, and timeouts last at least a minute", () => {
	assert.equal(retryStrategy("", ""), undefined);
	assert.deepEqual(retryStrategy(" 2 ", ""), { Attempts: 2 });
	assert.deepEqual(
		retryStrategy("3", '[{"OnExitCode":"137","Action":"RETRY"}]'),
		{ Attempts: 3, EvaluateOnExit: [{ OnExitCode: "137", Action: "RETRY" }] },
	);
	assert.throws(() => retryStrategy("", "[{}]"), /need a number of attempts/);
	assert.throws(() => retryStrategy("11", ""));
	assert.throws(() => retryStrategy("0", ""));
	assert.throws(() => retryStrategy("2", '{"a":1}'));
	assert.throws(() => retryStrategy("2", "[1]"));
	assert.throws(() => retryStrategy("2", JSON.stringify(Array(7).fill({}))));
	assert.equal(timeout(""), undefined);
	assert.deepEqual(timeout("60"), { AttemptDurationSeconds: 60 });
	assert.throws(() => timeout("59"), /Timeout/);
	assert.throws(() => timeout("1m"));
});

test("a definition registers container JSON on EC2 with its retry and timeout", () => {
	const form = definitionForm();
	assert.deepEqual(JSON.parse(form.container).Command, ["echo", "hello"]);
	assert.deepEqual(
		definitionInput({
			...form,
			name: "worker",
			parameters: '{"message":"hi"}',
			attempts: "2",
			timeout: "120",
			tags: '{"env":"test"}',
		}),
		{
			JobDefinitionName: "worker",
			Type: "container",
			PlatformCapabilities: ["EC2"],
			ContainerProperties: JSON.parse(form.container),
			Parameters: { message: "hi" },
			Tags: { env: "test" },
			RetryStrategy: { Attempts: 2 },
			Timeout: { AttemptDurationSeconds: 120 },
		},
	);
	const plain = definitionInput({ ...form, name: "plain" });
	assert.equal("RetryStrategy" in plain, false);
	assert.equal("Timeout" in plain, false);
	assert.throws(() => definitionInput({ ...form, name: "" }));
	assert.throws(() => definitionInput({ ...form, name: "w", container: "[]" }));
	assert.throws(() => definitionInput({ ...form, name: "w", container: "{" }));
	assert.throws(() =>
		definitionInput({ ...form, name: "w", parameters: '{"a":1}' }),
	);
});

test("the next revision starts from the definition it follows", () => {
	const form = definitionForm(definition);
	assert.equal(form.name, "worker");
	assert.deepEqual(JSON.parse(form.container), definition.ContainerProperties);
	assert.deepEqual(JSON.parse(form.parameters), { message: "hi" });
	assert.equal(form.attempts, "3");
	assert.deepEqual(JSON.parse(form.rules), [{ OnExitCode: "1" }]);
	assert.equal(form.timeout, "90");
	assert.deepEqual(JSON.parse(form.tags), { env: "test" });
	const out = definitionInput(form);
	assert.deepEqual(out.RetryStrategy, {
		Attempts: 3,
		EvaluateOnExit: [{ OnExitCode: "1" }],
	});
	assert.deepEqual(out.Timeout, { AttemptDurationSeconds: 90 });
});

test("vCPUs and memory come from requirements or the older members", () => {
	assert.deepEqual(resources(definition.ContainerProperties), {
		vcpus: 2,
		memory: 1024,
	});
	assert.deepEqual(resources({ Vcpus: 1, Memory: 256 }), {
		vcpus: 1,
		memory: 256,
	});
	assert.deepEqual(
		resources({ ResourceRequirements: [{ Type: "VCPU", Value: "4" }] }),
		{
			vcpus: 4,
			memory: undefined,
		},
	);
	assert.deepEqual(resources(undefined), {
		vcpus: undefined,
		memory: undefined,
	});
});

test("a job submits by name and revision with only the overrides given", () => {
	const form = (patch = {}) => ({
		...emptySubmitForm("q", "worker:3"),
		name: "run-1",
		...patch,
	});
	assert.deepEqual(submitInput(form(), [definition]), {
		JobName: "run-1",
		JobQueue: "q",
		JobDefinition: "worker:3",
	});
	assert.deepEqual(
		submitInput(
			form({
				parameters: '{"message":"go"}',
				command: '["echo","a b"]',
				environment: '{"STAGE":"dev"}',
				attempts: "2",
				timeout: "300",
				tags: '{"run":"1"}',
			}),
			[definition],
		),
		{
			JobName: "run-1",
			JobQueue: "q",
			JobDefinition: "worker:3",
			Parameters: { message: "go" },
			ContainerOverrides: {
				Command: ["echo", "a b"],
				Environment: [{ Name: "STAGE", Value: "dev" }],
			},
			RetryStrategy: { Attempts: 2 },
			Timeout: { AttemptDurationSeconds: 300 },
			Tags: { run: "1" },
		},
	);
});

test("a vCPU or memory override keeps the other from the definition", () => {
	const overrides = (patch: object, from = definition) =>
		submitInput({ ...emptySubmitForm("q", "worker:3"), name: "j", ...patch }, [
			from,
		]).ContainerOverrides;
	assert.deepEqual(overrides({ vcpus: "4" }), {
		ResourceRequirements: [
			{ Type: "VCPU", Value: "4" },
			{ Type: "MEMORY", Value: "1024" },
		],
	});
	assert.deepEqual(overrides({ memory: "2048" }), {
		ResourceRequirements: [
			{ Type: "VCPU", Value: "2" },
			{ Type: "MEMORY", Value: "2048" },
		],
	});
	// A definition with the older members still gets both requirements.
	const legacy = {
		...definition,
		ContainerProperties: { Image: "busybox", Vcpus: 1, Memory: 128 },
	};
	assert.deepEqual(overrides({ vcpus: "2" }, legacy), {
		ResourceRequirements: [
			{ Type: "VCPU", Value: "2" },
			{ Type: "MEMORY", Value: "128" },
		],
	});
	assert.throws(() => overrides({ vcpus: "0" }), /vCPUs/);
	assert.throws(() => overrides({ memory: "3" }), /Memory/);
	assert.throws(
		() =>
			overrides(
				{ vcpus: "2" },
				{ ...definition, ContainerProperties: { Image: "x" } },
			),
		/both vCPUs and memory/,
	);
});

test("a job can be an array that waits on other jobs", () => {
	const submit = (patch: object) =>
		submitInput({ ...emptySubmitForm("q", "worker:3"), name: "j", ...patch }, [
			definition,
		]);
	assert.deepEqual(submit({ size: "3", sequential: true, depends: "a1, b2" }), {
		JobName: "j",
		JobQueue: "q",
		JobDefinition: "worker:3",
		ArrayProperties: { Size: 3 },
		DependsOn: [{ JobId: "a1" }, { JobId: "b2" }, { Type: "SEQUENTIAL" }],
	});
	assert.throws(() => submit({ sequential: true }), /array size/);
	assert.throws(() => submit({ size: "1" }));
	assert.throws(() => submit({ size: "10001" }));
	assert.throws(() =>
		submit({
			depends: Array.from({ length: 21 }, (_, i) => `j${i}`).join(" "),
		}),
	);
	assert.equal(
		(
			submit({
				depends: Array.from({ length: 20 }, (_, i) => `j${i}`).join(" "),
			}).DependsOn ?? []
		).length,
		20,
	);
});

test("a job needs a name, a queue, an active definition and valid overrides", () => {
	const base = { ...emptySubmitForm("q", "worker:3"), name: "j" };
	for (const patch of [
		{ name: "" },
		{ name: "bad name" },
		{ queue: "" },
		{ definition: "" },
		{ definition: "worker:2" },
		{ parameters: "[]" },
		{ command: '"echo"' },
		{ command: "[1]" },
		{ environment: '{"1BAD":"x"}' },
		{ environment: '{"AWS_BATCH_X":"x"}' },
		{ environment: '{"A":1}' },
		{ attempts: "11" },
		{ timeout: "30" },
		{ tags: "{" },
	])
		assert.throws(
			() => submitInput({ ...base, ...patch }, [definition]),
			Error,
			JSON.stringify(patch),
		);
	assert.equal(definitionKey(definition), "worker:3");
});

test("definitions list by name with the newest revision first, jobs newest first", () => {
	const d = (JobDefinitionName: string, Revision: number) =>
		({ JobDefinitionName, Revision }) as JobDefinition;
	assert.deepEqual(
		sortDefinitions([d("b", 1), d("a", 1), d("a", 3), d("a", 2)]).map(
			definitionKey,
		),
		["a:3", "a:2", "a:1", "b:1"],
	);
	assert.deepEqual(
		sortJobs([{ CreatedAt: 1 }, { CreatedAt: 3 }, {}, { CreatedAt: 2 }]).map(
			(j) => j.CreatedAt,
		),
		[3, 2, 1, undefined],
	);
});

test("a job logs to its configured group or the default one", () => {
	const job = (Container: Job["Container"]) =>
		({ JobId: "1", Container }) as Job;
	assert.equal(jobLogGroup(job(undefined)), "/aws/batch/job");
	assert.equal(jobLogGroup(job({ LogConfiguration: {} })), "/aws/batch/job");
	assert.equal(
		jobLogGroup(
			job({
				LogConfiguration: {
					LogDriver: "awslogs",
					Options: { "awslogs-group": "/custom" },
				},
			}),
		),
		"/custom",
	);
});

test("ARNs show their name", () => {
	assert.equal(arnName(arn("job-queue", "q")), "q");
	assert.equal(arnName(arn("job-definition", "worker:3")), "worker:3");
	assert.equal(arnName("plain"), "plain");
	assert.equal(arnName(undefined), "-");
});

test("lists and command arrays are read as typed", () => {
	assert.deepEqual(words("a, b\nc  d"), ["a", "b", "c", "d"]);
	assert.deepEqual(words(" , "), []);
	assert.deepEqual(stringList('["a","b"]', "Command"), ["a", "b"]);
	assert.deepEqual(stringList("[]", "Command"), []);
	assert.throws(() => stringList("a", "Command"), /Command must be valid JSON/);
	assert.throws(
		() => stringList('{"a":"b"}', "Command"),
		/array of up to 100 strings/,
	);
	assert.throws(() => stringList("[1]", "Command"));
	assert.throws(() =>
		stringList(JSON.stringify(Array(101).fill("a")), "Command"),
	);
});

test("millisecond timestamps use the viewer's locale and a dash when absent", () => {
	assert.equal(unixMillis(undefined), "-");
	assert.equal(unixMillis(0), "-");
	assert.equal(
		unixMillis(1_700_000_000_123),
		new Date(1_700_000_000_123).toLocaleString(),
	);
});
