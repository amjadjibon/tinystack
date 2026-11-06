import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";
import { service } from "../src/lib/services.ts";
import {
	arn,
	type Check,
	type Context,
	check,
	emptySnapshot,
	type Snapshot,
} from "../src/tutorials/checks.ts";
import { HOTSPOT } from "../src/tutorials/hotspots.ts";
import { tutorials } from "../src/tutorials/index.ts";
import {
	clearProgress,
	fresh,
	load,
	restart,
	type Store,
	save,
} from "../src/tutorials/progress.ts";

test("definitions and rendered hotspot registry are complete", () => {
	assert.equal(new Set(tutorials.map((t) => t.id)).size, tutorials.length);
	const ids = new Set<string>();
	for (const t of tutorials) {
		assert.match(t.id, /^[a-z0-9]+(?:-[a-z0-9]+)+$/);
		assert.ok(t.tasks.length);
		for (const s of t.services) assert.notEqual(service(s).title, s);
		for (const task of t.tasks) {
			assert.ok(task.steps.length);
			for (const s of task.steps) {
				assert.ok(
					s.title &&
						s.content &&
						(s.route.startsWith("/services/") || s.route === "/mailbox") &&
						s.command,
				);
				assert.ok(Object.values(HOTSPOT).includes(s.hotspotId));
				ids.add(s.hotspotId);
			}
		}
	}
	const rendered = new Set<string>();
	function scan(dir: string) {
		for (const entry of readdirSync(dir, { withFileTypes: true })) {
			const path = join(dir, entry.name);
			if (entry.isDirectory()) scan(path);
			else if (path.endsWith(".tsx")) {
				const text = readFileSync(path, "utf8");
				for (const m of text.matchAll(/hotspotId=\{HOTSPOT\.(\w+)\}/g)) {
					assert.ok(m[1] in HOTSPOT);
					rendered.add(HOTSPOT[m[1] as keyof typeof HOTSPOT]);
				}
				assert.doesNotMatch(text, /hotspotId=["']/);
			}
		}
	}
	scan(new URL("../src", import.meta.url).pathname);
	assert.deepEqual([...ids].sort(), Object.values(HOTSPOT).sort());
	assert.deepEqual([...rendered].sort(), [...ids].sort());
});
const ctx: Context = {
	account: "000000000000",
	region: "us-east-1",
	names: {
		bucket: "tutorial-s3-1234",
		key: "hello.txt",
		topic: "tutorial-sns-1234",
		queue: "tutorial-sqs-1234",
		table: "tutorial-ddb-1234",
		rule: "tutorial-rule-1234",
	},
};
function fixture(kind: Check): Snapshot {
	const s = emptySnapshot();
	const c = ctx;
	const n = c.names;
	if (kind === "table")
		s.resources.push({
			service: "dynamodb",
			type: "table",
			account: c.account,
			region: c.region,
			name: n.table,
			arn: arn("dynamodb", `table/${n.table}`, c),
		});
	else if (kind === "rule" || kind === "target")
		s.resources.push({
			service: "eventbridge",
			type: "rule",
			account: c.account,
			region: c.region,
			name: n.rule,
			arn: arn("events", `rule/${n.rule}`, c),
			details: {
				Rule: { Name: n.rule, EventBusName: "default" },
				Targets: [{ ID: "queue", ARN: arn("sqs", n.queue, c) }],
			},
		});
	else if (kind === "event")
		s.deliveries.push({
			time: "",
			via: "eventbridge",
			source: arn("events", `rule/${n.rule}`, c),
			target: arn("sqs", n.queue, c),
			status: "delivered",
			payload: "",
			size: 0,
		});
	else if (kind === "item" || kind === "read")
		s.requests.push({
			time: "",
			request_id: "",
			method: "POST",
			path: "/",
			account: c.account,
			region: c.region,
			service: "dynamodb",
			operation: kind === "item" ? "PutItem" : "Scan",
			status: 200,
			request_body: JSON.stringify({ TableName: n.table }),
			response_body: JSON.stringify({
				Items: [{ pk: { S: "hello" } }],
				Count: 1,
			}),
			request_size: 0,
			response_size: 0,
			duration_ms: 0,
		});
	else if (["bucket", "topic", "queue", "subscription"].includes(kind)) {
		const name =
			kind === "bucket" ? n.bucket : kind === "queue" ? n.queue : n.topic;
		const svc = kind === "bucket" ? "s3" : kind === "queue" ? "sqs" : "sns";
		s.resources.push({
			service: svc,
			type: kind,
			account: c.account,
			region: c.region,
			name,
			arn: arn(svc, name, c),
			details: {
				Subscriptions: [
					{
						SubscriptionArn: `${arn("sns", n.topic, c)}:id`,
						Protocol: "sqs",
						Endpoint: arn("sqs", n.queue, c),
					},
				],
			},
		});
	} else if (kind === "delivery")
		s.deliveries.push({
			time: "",
			via: "sns",
			source: arn("sns", n.topic, c),
			target: arn("sqs", n.queue, c),
			status: "delivered",
			payload: "",
			size: 0,
		});
	else
		s.requests.push({
			time: "",
			request_id: "",
			method: "POST",
			path: "/_tinystack/actions",
			account: c.account,
			region: c.region,
			service: kind === "receive" ? "sqs" : "s3",
			operation:
				kind === "object"
					? "PutObject"
					: kind === "download"
						? "GetObject"
						: "ReceiveMessage",
			status: 200,
			request_body: JSON.stringify({
				input:
					kind === "receive"
						? { QueueUrl: `/${c.account}/${n.queue}` }
						: { Bucket: n.bucket, Key: n.key },
			}),
			response_body: JSON.stringify({
				output: { Messages: [{ Body: "Hello" }] },
			}),
			request_size: 0,
			response_size: 0,
			duration_ms: 0,
		});
	return s;
}
for (const kind of [
	"bucket",
	"object",
	"download",
	"topic",
	"queue",
	"subscription",
	"delivery",
	"receive",
	"table",
	"item",
	"read",
	"rule",
	"target",
	"event",
] as const) {
	test(`${kind}: pass, absent, other account, Region and names`, () => {
		const s = fixture(kind);
		assert.equal(check(kind, s, ctx), true);
		assert.equal(check(kind, emptySnapshot(), ctx), false);
		assert.equal(check(kind, s, { ...ctx, account: "111111111111" }), false);
		assert.equal(check(kind, s, { ...ctx, region: "eu-west-1" }), false);
		assert.equal(
			check(kind, s, {
				...ctx,
				names: {
					bucket: "other",
					key: "other",
					topic: "other",
					queue: "other",
					table: "other",
					rule: "other",
				},
			}),
			false,
		);
		if (s.requests.length) {
			s.requests[0].status = 403;
			assert.equal(check(kind, s, ctx), false);
		}
		if (s.deliveries.length) {
			s.deliveries[0].status = "failed";
			assert.equal(check(kind, s, ctx), false);
		}
	});
}
test("wire S3 requests match exact key and failed or empty receives do not pass", () => {
	const s = fixture("download");
	s.requests[0].request_body = "";
	s.requests[0].path = `/${ctx.names.bucket}/${ctx.names.key}`;
	assert.ok(check("download", s, ctx));
	s.requests[0].path += "-other";
	assert.equal(check("download", s, ctx), false);
	const r = fixture("receive");
	r.requests[0].response_body = '{"output":{"Messages":[]}}';
	assert.equal(check("receive", r, ctx), false);
});
test("progress survives reload, isolates scopes, restarts and resets only tutorial keys", () => {
	const data = new Map<string, string>();
	const store: Store = {
		getItem: (k) => data.get(k) ?? null,
		setItem: (k, v) => {
			data.set(k, v);
		},
		removeItem: (k) => {
			data.delete(k);
		},
		key: (i) => [...data.keys()][i] ?? null,
		get length() {
			return data.size;
		},
	};
	const p = { ...fresh("s3-first-bucket"), step: 2, completed: [0, 1] };
	save(store, "s3-first-bucket", ctx.account, ctx.region, p);
	assert.deepEqual(load(store, "s3-first-bucket", ctx.account, ctx.region), p);
	assert.equal(load(store, "sns-sqs-fanout", ctx.account, ctx.region), null);
	assert.equal(load(store, "s3-first-bucket", "other", ctx.region), null);
	assert.equal(load(store, "s3-first-bucket", ctx.account, "other"), null);
	restart(store, "s3-first-bucket", ctx.account, ctx.region);
	assert.equal(load(store, "s3-first-bucket", ctx.account, ctx.region), null);
	save(store, "s3-first-bucket", ctx.account, ctx.region, p);
	save(store, "sns-sqs-fanout", ctx.account, "eu-west-1", p);
	store.setItem("unrelated", "keep");
	clearProgress(store);
	assert.equal(store.length, 1);
	assert.equal(store.getItem("unrelated"), "keep");
});

test("copied step commands work in CloudShell without a host shell", () => {
	for (const tutorial of tutorials)
		for (const task of tutorial.tasks)
			for (const step of task.steps) {
				assert.match(step.command, /^aws-local /);
				assert.doesNotMatch(
					step.command,
					/\n|\$\(|\$[A-Z]|\bexport\b|\bprintf\b|[<>|]/,
				);
			}
});

test("rules on other buses, missing targets and empty reads do not pass", () => {
	const r = fixture("rule");
	(
		r.resources[0].details as { Rule: { EventBusName: string } }
	).Rule.EventBusName = "orders";
	assert.equal(check("rule", r, ctx), false);
	const t = fixture("target");
	(t.resources[0].details as { Targets: unknown[] }).Targets = [];
	assert.equal(check("rule", t, ctx), true);
	assert.equal(check("target", t, ctx), false);
	const read = fixture("read");
	read.requests[0].response_body = '{"Items":[],"Count":0}';
	assert.equal(check("read", read, ctx), false);
	read.requests[0].operation = "GetItem";
	read.requests[0].request_body = JSON.stringify({
		input: { TableName: ctx.names.table },
	});
	read.requests[0].response_body = JSON.stringify({
		output: { Item: { pk: { S: "hello" } } },
	});
	assert.ok(check("read", read, ctx));
});

test("cleanup plans name only generated resources, in dependency order", async () => {
	const calls: string[] = [];
	const call = async <T>(
		service: string,
		operation: string,
		input: Record<string, unknown>,
	): Promise<T> => {
		calls.push(`${service}:${operation}:${JSON.stringify(input)}`);
		if (operation === "ListTargetsByRule")
			return {
				Targets: [{ Id: "queue", Arn: arn("sqs", ctx.names.queue, ctx) }],
			} as T;
		if (operation === "ListSubscriptionsByTopic")
			return {
				Subscriptions: [
					{
						SubscriptionArn: "sub-1",
						Protocol: "sqs",
						Endpoint: arn("sqs", ctx.names.queue, ctx),
					},
					{
						SubscriptionArn: "sub-other",
						Protocol: "sqs",
						Endpoint: arn("sqs", "other", ctx),
					},
				],
			} as T;
		return {} as T;
	};
	const plans = Object.fromEntries(
		await Promise.all(
			tutorials.map(async (t) => [t.id, await t.cleanup(ctx, call)] as const),
		),
	);
	calls.length = 0;
	for (const step of plans["eventbridge-sqs-rule"]) await step.run();
	assert.deepEqual(
		calls.map((c) => c.split(":").slice(0, 2).join(":")),
		["eventbridge:RemoveTargets", "eventbridge:DeleteRule", "sqs:DeleteQueue"],
	);
	assert.match(calls[1], new RegExp(ctx.names.rule));
	assert.deepEqual(
		plans["sns-sqs-fanout"].map((s) => s.label.split(" ")[0]),
		["Subscription", "Topic", "Queue"],
	);
	assert.ok(
		!plans["sns-sqs-fanout"].some((s) => s.label.includes("sub-other")),
	);
	calls.length = 0;
	for (const step of plans["dynamodb-first-table"]) await step.run();
	assert.deepEqual(calls, [
		`dynamodb:DeleteTable:${JSON.stringify({ TableName: ctx.names.table })}`,
	]);
});

const runtimeContext: Context = {
	account: ctx.account,
	region: ctx.region,
	names: {
		function: "tutorial-function",
		machine: "tutorial-machine",
		execution: "tutorial-execution",
		secret: "tutorial-secret",
		parameter: "tutorial-parameter",
	},
};
function runtimeFixture(kind: Check): Snapshot {
	const s = emptySnapshot(),
		c = runtimeContext,
		n = c.names;
	const svc =
		kind === "function"
			? "lambda"
			: kind === "logStream"
				? "logs"
				: kind === "secret" || kind === "secretValue"
					? "secretsmanager"
					: kind === "parameter"
						? "ssm"
						: "sfn";
	const name =
		svc === "lambda"
			? n.function
			: svc === "logs"
				? `/aws/lambda/${n.function}`
				: svc === "secretsmanager"
					? n.secret
					: svc === "ssm"
						? n.parameter
						: n.machine;
	if (
		[
			"function",
			"logStream",
			"stateMachine",
			"executionStarted",
			"executionSucceeded",
			"secret",
			"secretValue",
			"parameter",
		].includes(kind)
	)
		s.resources.push({
			service: svc,
			type:
				svc === "lambda"
					? "function"
					: svc === "logs"
						? "log-group"
						: svc === "secretsmanager"
							? "secret"
							: svc === "ssm"
								? "parameter"
								: "state-machine",
			name,
			account: c.account,
			region: c.region,
			arn: arn(svc, `secret:${name}-abc123`, c),
			details: {
				Streams: ["stream"],
				Executions: [{ Name: n.execution, Status: "SUCCEEDED" }],
			},
		});
	const requests: Partial<Record<Check, [string, string, string]>> = {
		invoke: ["lambda", "Invoke", n.function],
		secretValue: [
			"secretsmanager",
			"GetSecretValue",
			s.resources[0]?.arn ?? "",
		],
		parameterValue: ["ssm", "GetParameter", `${n.parameter}:1`],
		executionSucceeded: [
			"sfn",
			"DescribeExecution",
			arn("states", `execution:${n.machine}:${n.execution}`, c),
		],
		logStream: ["logs", "DescribeLogStreams", ""],
	};
	const request = requests[kind];
	if (request)
		s.requests.push({
			...fixture("item").requests[0],
			service: request[0],
			operation: request[1],
			resource_identifier: request[2],
			request_body: "(redacted: this operation carries sensitive values)",
			response_body: "(redacted: this operation carries sensitive values)",
		});
	// DescribeLogStreams is not sensitive, so its wire body is logged as sent.
	if (kind === "logStream")
		s.requests[0].request_body = JSON.stringify({ logGroupName: name });
	return s;
}
for (const kind of [
	"function",
	"invoke",
	"logStream",
	"stateMachine",
	"executionStarted",
	"executionSucceeded",
	"secret",
	"secretValue",
	"parameter",
	"parameterValue",
] as const) {
	test(`${kind}: scoped records pass; absent, failed and unrelated records fail`, () => {
		const c = runtimeContext,
			s = runtimeFixture(kind);
		assert.ok(check(kind, s, c));
		assert.equal(check(kind, emptySnapshot(), c), false);
		assert.equal(check(kind, s, { ...c, account: "111111111111" }), false);
		assert.equal(check(kind, s, { ...c, region: "eu-west-1" }), false);
		assert.equal(
			check(kind, s, {
				...c,
				names: Object.fromEntries(
					Object.keys(c.names).map((k) => [k, "other"]),
				),
			}),
			false,
		);
		for (const r of s.requests) r.status = 403;
		if (s.requests.length) assert.equal(check(kind, s, c), false);
	});
}
test("runtime state checks reject empty streams, wrong execution and non-success statuses", () => {
	const s = runtimeFixture("logStream");
	s.resources[0].details = { Streams: [] };
	assert.equal(check("logStream", s, runtimeContext), false);
	for (const Status of ["RUNNING", "FAILED", "TIMED_OUT", "ABORTED"]) {
		const e = runtimeFixture("executionSucceeded");
		e.resources[0].details = {
			Executions: [{ Name: runtimeContext.names.execution, Status }],
		};
		assert.equal(check("executionSucceeded", e, runtimeContext), false);
	}
	const e = runtimeFixture("executionSucceeded");
	e.resources[0].details = {
		Executions: [{ Name: "other", Status: "SUCCEEDED" }],
	};
	assert.equal(check("executionSucceeded", e, runtimeContext), false);
});
test("log and execution steps wait for someone to read them, from the CLI or the console", () => {
	for (const kind of ["logStream", "executionSucceeded"] as const) {
		const s = runtimeFixture(kind);
		s.requests.length = 0;
		assert.equal(check(kind, s, runtimeContext), false);
	}
	const s = runtimeFixture("logStream");
	s.requests[0].request_body = JSON.stringify({
		service: "logs",
		operation: "DescribeLogStreams",
		input: { LogGroupName: `/aws/lambda/${runtimeContext.names.function}` },
	});
	assert.equal(check("logStream", s, runtimeContext), true);
	s.requests[0].request_body = JSON.stringify({
		input: { LogGroupName: "/aws/lambda/other" },
	});
	assert.equal(check("logStream", s, runtimeContext), false);
});
test("redacted requests without safe identifiers cannot complete a resource-specific step", () => {
	for (const kind of ["invoke", "secretValue", "parameterValue"] as const) {
		const s = runtimeFixture(kind);
		delete s.requests[0].resource_identifier;
		assert.equal(check(kind, s, runtimeContext), false);
		s.requests[0].resource_identifier = "other";
		assert.equal(check(kind, s, runtimeContext), false);
	}
});
test("runtime tutorial cleanup targets only generated names in order", async () => {
	const expected: Record<string, [string, string, Record<string, unknown>][]> =
		{
			"lambda-first-function": [
				[
					"lambda",
					"DeleteFunction",
					{ FunctionName: runtimeContext.names.function },
				],
				[
					"logs",
					"DeleteLogGroup",
					{ LogGroupName: `/aws/lambda/${runtimeContext.names.function}` },
				],
			],
			"sfn-first-state-machine": [
				[
					"sfn",
					"DeleteStateMachine",
					{
						StateMachineArn: arn(
							"states",
							`stateMachine:${runtimeContext.names.machine}`,
							runtimeContext,
						),
					},
				],
			],
			"secrets-and-parameters": [
				[
					"secretsmanager",
					"DeleteSecret",
					{
						SecretId: runtimeContext.names.secret,
						ForceDeleteWithoutRecovery: true,
					},
				],
				["ssm", "DeleteParameter", { Name: runtimeContext.names.parameter }],
			],
		};
	for (const [id, calls] of Object.entries(expected)) {
		const actual: unknown[] = [];
		const plan = await tutorials
			.find((t) => t.id === id)
			?.cleanup(
				runtimeContext,
				async <T>(s: string, o: string, i: Record<string, unknown>) => {
					actual.push([s, o, i]);
					return {} as T;
				},
			);
		assert.ok(plan);
		for (const row of plan) await row.run();
		assert.deepEqual(actual, calls);
	}
});
test("runtime tutorials receive generated names", () => {
	const n = fresh("lambda-first-function").names;
	for (const key of ["function", "machine", "execution", "secret", "parameter"])
		assert.match(n[key], /^tutorial-lambda-first-function-[a-f0-9]{4}$/);
});

const t3ctx: Context = {
	...ctx,
	names: { ...ctx.names, ...fresh("t3-records").names },
};
function t3fixture(): Snapshot {
	const c = t3ctx,
		n = c.names,
		s = emptySnapshot();
	const poolArn = arn("cognito-idp", "userpool/us-east-1_pool", c);
	s.resources.push(
		{
			service: "iam",
			type: "role",
			account: c.account,
			region: "",
			name: n.role,
			arn: `arn:aws:iam::${c.account}:role/${n.role}`,
			details: { InlinePolicies: [n.policy] },
		},
		{
			service: "cognitoidp",
			type: "user-pool",
			account: c.account,
			region: c.region,
			name: n.pool,
			arn: poolArn,
			details: { Id: "us-east-1_pool", Clients: 1, Users: 1 },
		},
		{
			service: "cloudformation",
			type: "stack",
			account: c.account,
			region: c.region,
			name: n.stack,
			arn: arn("cloudformation", `stack/${n.stack}/id`, c),
			details: { Status: "CREATE_COMPLETE" },
		},
	);
	s.messages.push({
		id: "mail",
		time: "",
		service: "cognito-idp",
		channel: "email",
		account: c.account,
		region: c.region,
		source: poolArn,
		to: n.email,
		subject: "Your verification code",
		body: "Your verification code is 123456. ",
	});
	const request = (
		service: string,
		operation: string,
		input: Record<string, unknown>,
		output: Record<string, unknown>,
	) =>
		s.requests.push({
			...fixture("read").requests[0],
			service,
			operation,
			request_body: JSON.stringify({ input }),
			response_body: JSON.stringify({ output }),
		});
	request(
		"iam",
		"PutRolePolicy",
		{
			RoleName: n.role,
			PolicyName: n.policy,
			PolicyDocument: JSON.stringify({
				Version: "2012-10-17",
				Statement: [
					{
						Effect: "Allow",
						Action: "s3:GetObject",
						Resource: `arn:aws:s3:::${n.bucket}/${n.key}`,
					},
				],
			}),
		},
		{},
	);
	request(
		"iam",
		"SimulatePrincipalPolicy",
		{ PolicySourceArn: `arn:aws:iam::${c.account}:role/${n.role}` },
		{
			EvaluationResults: [
				{
					EvalActionName: "s3:GetObject",
					EvalResourceName: `arn:aws:s3:::${n.bucket}/${n.key}`,
					EvalDecision: "allowed",
				},
				{
					EvalActionName: "s3:DeleteObject",
					EvalResourceName: `arn:aws:s3:::${n.bucket}/${n.key}`,
					EvalDecision: "implicitDeny",
				},
			],
		},
	);
	request(
		"cloudformation",
		"DescribeStacks",
		{ StackName: s.resources[2].arn },
		{
			Stacks: [
				{
					StackName: n.stack,
					Outputs: [
						{
							OutputKey: "QueueUrl",
							OutputValue: `http://localhost:4702/${c.account}/${n.queue}`,
						},
					],
				},
			],
		},
	);
	return s;
}
for (const kind of [
	"iamRole",
	"iamPolicy",
	"iamSimulation",
	"cognitoPool",
	"cognitoClient",
	"cognitoSignup",
	"cfnStack",
	"cfnOutputs",
	"cfnUpdated",
] as const) {
	test(`${kind}: observed record shape, absent, wrong scope and generated names`, () => {
		const s = t3fixture();
		if (kind === "cfnUpdated")
			s.resources[2].details = { Status: "UPDATE_COMPLETE" };
		assert.ok(check(kind, s, t3ctx));
		assert.equal(check(kind, emptySnapshot(), t3ctx), false);
		assert.equal(check(kind, s, { ...t3ctx, account: "111111111111" }), false);
		assert.equal(
			check(kind, s, {
				...t3ctx,
				names: Object.fromEntries(
					Object.keys(t3ctx.names).map((k) => [k, "other"]),
				),
			}),
			false,
		);
		// IAM inventory and console calls are global; selected Region does not partition them.
		assert.equal(
			check(kind, s, { ...t3ctx, region: "eu-west-1" }),
			["iamRole", "iamPolicy", "iamSimulation"].includes(kind),
		);
		if (kind === "iamRole") {
			s.resources[0].region = t3ctx.region;
			assert.equal(check(kind, s, t3ctx), false);
		}
		if (["iamPolicy", "iamSimulation", "cfnOutputs"].includes(kind)) {
			for (const r of s.requests) r.status = 403;
			assert.equal(check(kind, s, t3ctx), false);
		}
	});
}
test("Cognito confirmation needs a successful ConfirmSignUp for the generated user", () => {
	const s = t3fixture();
	for (const operation of ["SignUp", "ConfirmSignUp", "AdminGetUser"])
		s.requests.push({
			...fixture("read").requests[0],
			service: "cognitoidp",
			operation,
			request_body: "(redacted: this operation carries sensitive values)",
			response_body: "(redacted: this operation carries sensitive values)",
		});
	assert.equal(check("cognitoConfirmed", s, t3ctx), false);
	const confirm = s.requests[s.requests.length - 2];
	confirm.resource_identifier = "another-user";
	assert.equal(check("cognitoConfirmed", s, t3ctx), false);
	confirm.resource_identifier = t3ctx.names.username;
	assert.equal(check("cognitoConfirmed", s, t3ctx), true);
	assert.equal(
		check("cognitoConfirmed", s, { ...t3ctx, region: "eu-west-1" }),
		false,
	);
	confirm.status = 400;
	assert.equal(check("cognitoConfirmed", s, t3ctx), false);
	const pool = s.resources[1];
	pool.details = { Clients: 0, Users: 0 };
	assert.equal(check("cognitoClient", s, t3ctx), false);
	assert.equal(check("cognitoSignup", s, t3ctx), false);
	pool.details = { Clients: 1, Users: 1 };
	assert.equal(check("cognitoSignup", s, t3ctx), true);
	s.messages[0].source = arn("cognito-idp", "userpool/another", t3ctx);
	assert.equal(check("cognitoSignup", s, t3ctx), false);
});
test("IAM simulation requires both decisions on the exact object and a narrow policy", () => {
	const s = t3fixture();
	s.requests[1].response_body = JSON.stringify({
		output: {
			EvaluationResults: [
				{
					EvalActionName: "s3:GetObject",
					EvalResourceName: `arn:aws:s3:::${t3ctx.names.bucket}/${t3ctx.names.key}`,
					EvalDecision: "allowed",
				},
			],
		},
	});
	assert.equal(check("iamSimulation", s, t3ctx), false);
	const input = JSON.parse(s.requests[0].request_body).input;
	input.PolicyDocument = JSON.stringify({
		Statement: [{ Effect: "Allow", Action: "*", Resource: "*" }],
	});
	s.requests[0].request_body = JSON.stringify({ input });
	assert.equal(check("iamPolicy", s, t3ctx), false);
});
test("T3 cleanup deletes the generated inline policy before the role, and only its pool and stack", async () => {
	const calls: {
		service: string;
		operation: string;
		input: Record<string, unknown>;
	}[] = [];
	const call = async <T>(
		service: string,
		operation: string,
		input: Record<string, unknown>,
	): Promise<T> => {
		calls.push({ service, operation, input });
		if (operation === "ListUserPools")
			return (
				input.NextToken
					? { UserPools: [{ Name: t3ctx.names.pool, Id: "generated-pool" }] }
					: {
							UserPools: [{ Name: "unrelated", Id: "other" }],
							NextToken: "next",
						}
			) as T;
		return {} as T;
	};
	for (const id of [
		"iam-least-privilege",
		"cognito-sign-up",
		"cloudformation-first-stack",
	]) {
		const tutorial = tutorials.find((t) => t.id === id);
		assert.ok(tutorial);
		for (const step of await tutorial.cleanup(t3ctx, call)) await step.run();
	}
	assert.deepEqual(calls, [
		{
			service: "iam",
			operation: "DeleteRolePolicy",
			input: { RoleName: t3ctx.names.role, PolicyName: t3ctx.names.policy },
		},
		{
			service: "iam",
			operation: "DeleteRole",
			input: { RoleName: t3ctx.names.role },
		},
		{
			service: "cognitoidp",
			operation: "ListUserPools",
			input: { MaxResults: 60 },
		},
		{
			service: "cognitoidp",
			operation: "ListUserPools",
			input: { MaxResults: 60, NextToken: "next" },
		},
		{
			service: "cognitoidp",
			operation: "DeleteUserPool",
			input: { UserPoolId: "generated-pool" },
		},
		{
			service: "cloudformation",
			operation: "DeleteStack",
			input: { StackName: t3ctx.names.stack },
		},
	]);
});

test("Query protocol IAM writes, simulation and CloudFormation outputs match wire records", () => {
	const s = t3fixture(),
		n = t3ctx.names;
	const policy = JSON.parse(s.requests[0].request_body).input;
	s.requests[0].request_body = new URLSearchParams(policy).toString();
	assert.ok(check("iamPolicy", s, t3ctx));
	s.requests[1].request_body = new URLSearchParams({
		PolicySourceArn: `arn:aws:iam::${t3ctx.account}:role/${n.role}`,
	}).toString();
	const result = (action: string, decision: string) =>
		`<member><EvalActionName>${action}</EvalActionName><EvalResourceName>arn:aws:s3:::${n.bucket}/${n.key}</EvalResourceName><EvalDecision>${decision}</EvalDecision></member>`;
	s.requests[1].response_body = `<SimulatePrincipalPolicyResponse><EvaluationResults>${result("s3:GetObject", "allowed")}${result("s3:DeleteObject", "implicitDeny")}</EvaluationResults></SimulatePrincipalPolicyResponse>`;
	assert.ok(check("iamSimulation", s, t3ctx));
	s.requests[1].response_body = s.requests[1].response_body.replace(
		"implicitDeny",
		"allowed",
	);
	assert.equal(check("iamSimulation", s, t3ctx), false);
	s.requests[2].request_body = new URLSearchParams({
		StackName: n.stack,
	}).toString();
	s.requests[2].response_body = `<DescribeStacksResponse><Stacks><member><StackName>${n.stack}</StackName><Outputs><member><OutputKey>QueueUrl</OutputKey><OutputValue>http://localhost:4702/${t3ctx.account}/${n.queue}</OutputValue></member></Outputs></member></Stacks></DescribeStacksResponse>`;
	assert.ok(check("cfnOutputs", s, t3ctx));
	s.requests[2].response_body = s.requests[2].response_body.replace(
		n.queue,
		"other",
	);
	assert.equal(check("cfnOutputs", s, t3ctx), false);
});
