import type { Delivery, RequestRecord, Resource } from "../lib/api.ts";
import type { MailMessage } from "../lib/mailboxHelpers.ts";
export type Snapshot = {
	resources: Resource[];
	requests: RequestRecord[];
	deliveries: Delivery[];
	messages: MailMessage[];
};
export type Context = {
	account: string;
	region: string;
	names: Record<string, string>;
};
export type Check =
	| "bucket"
	| "object"
	| "download"
	| "topic"
	| "queue"
	| "subscription"
	| "delivery"
	| "receive"
	| "table"
	| "item"
	| "read"
	| "rule"
	| "target"
	| "event"
	| "function"
	| "invoke"
	| "logStream"
	| "stateMachine"
	| "executionStarted"
	| "executionSucceeded"
	| "secret"
	| "secretValue"
	| "parameter"
	| "parameterValue"
	| "iamRole"
	| "iamPolicy"
	| "iamSimulation"
	| "cognitoPool"
	| "cognitoClient"
	| "cognitoSignup"
	| "cognitoMail"
	| "cognitoConfirmed"
	| "cfnStack"
	| "cfnCreated"
	| "cfnOutputs"
	| "cfnUpdated";
export const sources: Record<Check, (keyof Snapshot)[]> = {
	bucket: ["resources"],
	object: ["requests"],
	download: ["requests"],
	topic: ["resources"],
	queue: ["resources"],
	subscription: ["resources"],
	delivery: ["deliveries"],
	receive: ["requests"],
	table: ["resources"],
	item: ["requests"],
	read: ["requests"],
	rule: ["resources"],
	target: ["resources"],
	event: ["deliveries"],
	function: ["resources"],
	invoke: ["requests"],
	logStream: ["resources"],
	stateMachine: ["resources"],
	executionStarted: ["resources"],
	executionSucceeded: ["resources"],
	secret: ["resources"],
	secretValue: ["resources", "requests"],
	parameter: ["resources"],
	parameterValue: ["requests"],
	iamRole: ["resources"],
	iamPolicy: ["resources", "requests"],
	iamSimulation: ["requests"],
	cognitoPool: ["resources"],
	cognitoClient: ["resources"],
	cognitoSignup: ["resources", "messages"],
	cognitoMail: ["resources", "messages"],
	cognitoConfirmed: ["requests"],
	cfnStack: ["resources"],
	cfnCreated: ["resources"],
	cfnOutputs: ["resources", "requests"],
	cfnUpdated: ["resources"],
};
export const emptySnapshot = (): Snapshot => ({
	resources: [],
	requests: [],
	deliveries: [],
	messages: [],
});
export const arn = (service: string, name: string, c: Context) =>
	`arn:aws:${service}:${c.region}:${c.account}:${name}`;
export function resource(
	s: Snapshot,
	c: Context,
	service: string,
	name: string,
) {
	return s.resources.find(
		(r) =>
			r.account === c.account &&
			r.region === c.region &&
			r.service === service &&
			r.name === name,
	);
}
export function subscriptions(
	s: Snapshot,
	c: Context,
): {
	SubscriptionArn: string;
	Protocol: string;
	Endpoint: string;
	PendingConfirmation?: boolean;
}[] {
	const rows = resource(s, c, "sns", c.names.topic)?.details?.Subscriptions;
	return Array.isArray(rows) ? rows : [];
}
/** rule is the generated rule on the default event bus; a same-named rule on another bus does not count. */
function rule(s: Snapshot, c: Context) {
	const r = resource(s, c, "eventbridge", c.names.rule);
	const details = (r?.details ?? {}) as {
		Rule?: { EventBusName?: string };
		Targets?: { ARN?: string }[];
	};
	return r?.type === "rule" && details.Rule?.EventBusName === "default"
		? details
		: undefined;
}
function json(text: string): Record<string, unknown> {
	try {
		const value = JSON.parse(text);
		return value && typeof value === "object" ? value : {};
	} catch {
		return {};
	}
}
function successful(
	s: Snapshot,
	c: Context,
	service: string,
	operation: string,
	match: (r: RequestRecord, input: Record<string, unknown>) => boolean,
) {
	return s.requests.some((r) => {
		if (
			r.account !== c.account ||
			(service !== "iam" && r.region !== c.region) ||
			r.service !== service ||
			r.operation !== operation ||
			r.status < 200 ||
			r.status >= 300 ||
			r.error_code
		)
			return false;
		const body = r.request_body.trimStart().startsWith("{")
			? json(r.request_body)
			: Object.fromEntries(new URLSearchParams(r.request_body));
		const input =
			body.input && typeof body.input === "object"
				? (body.input as Record<string, unknown>)
				: body;
		return match(r, input);
	});
}
/** output reads a logged response from the console action envelope or the wire body. */
function output(r: RequestRecord): Record<string, unknown> {
	const body = json(r.response_body);
	return body.output && typeof body.output === "object"
		? (body.output as Record<string, unknown>)
		: body;
}
/** IAM inventory is global; regional records never stand in for a global role. */
function tutorialRole(s: Snapshot, c: Context) {
	return s.resources.find(
		(r) =>
			r.service === "iam" &&
			r.type === "role" &&
			r.account === c.account &&
			r.region === "" &&
			r.name === c.names.role,
	);
}
function tutorialPool(s: Snapshot, c: Context) {
	const r = resource(s, c, "cognitoidp", c.names.pool);
	return r?.type === "user-pool" ? r : undefined;
}
export function check(kind: Check, s: Snapshot, c: Context): boolean {
	const n = c.names;
	switch (kind) {
		case "bucket":
			return !!resource(s, c, "s3", n.bucket);
		case "topic":
			return !!resource(s, c, "sns", n.topic);
		case "queue":
			return !!resource(s, c, "sqs", n.queue);
		case "subscription":
			return subscriptions(s, c).some(
				(r) =>
					r.Protocol === "sqs" &&
					r.Endpoint === arn("sqs", n.queue, c) &&
					!r.PendingConfirmation,
			);
		case "delivery":
			return s.deliveries.some(
				(d) =>
					d.source === arn("sns", n.topic, c) &&
					d.target === arn("sqs", n.queue, c) &&
					d.status === "delivered" &&
					d.via === "sns" &&
					!d.error,
			);
		case "object":
		case "download":
			return successful(
				s,
				c,
				"s3",
				kind === "object" ? "PutObject" : "GetObject",
				(r, i) =>
					(i.Bucket === n.bucket && i.Key === n.key) ||
					r.path.split("?")[0] === `/${n.bucket}/${n.key}`,
			);
		case "receive":
			return successful(s, c, "sqs", "ReceiveMessage", (r, i) => {
				const url = String(i.QueueUrl ?? r.path).split("?")[0];
				const matches = url.endsWith(`/${c.account}/${n.queue}`);
				const out = output(r);
				return (
					matches &&
					((Array.isArray(out.Messages) && out.Messages.length > 0) ||
						/<Message>/.test(r.response_body))
				);
			});
		case "table":
			return !!resource(s, c, "dynamodb", n.table);
		case "item":
			return successful(
				s,
				c,
				"dynamodb",
				"PutItem",
				(_, i) => i.TableName === n.table,
			);
		case "read":
			return ["Scan", "Query", "GetItem"].some((operation) =>
				successful(s, c, "dynamodb", operation, (r, i) => {
					const out = output(r);
					return (
						i.TableName === n.table &&
						((Array.isArray(out.Items) && out.Items.length > 0) ||
							(!!out.Item &&
								typeof out.Item === "object" &&
								Object.keys(out.Item).length > 0))
					);
				}),
			);
		case "rule":
			return !!rule(s, c);
		case "target":
			return !!rule(s, c)?.Targets?.some(
				(t) => t.ARN === arn("sqs", n.queue, c),
			);
		case "event":
			return s.deliveries.some(
				(d) =>
					d.source === arn("events", `rule/${n.rule}`, c) &&
					d.target === arn("sqs", n.queue, c) &&
					d.status === "delivered" &&
					d.via === "eventbridge" &&
					!d.error,
			);
		case "function":
			return resource(s, c, "lambda", n.function)?.type === "function";
		case "logStream": {
			const r = resource(s, c, "logs", `/aws/lambda/${n.function}`);
			return (
				r?.type === "log-group" &&
				Array.isArray(r.details?.Streams) &&
				r.details.Streams.length > 0
			);
		}
		case "stateMachine":
			return resource(s, c, "sfn", n.machine)?.type === "state-machine";
		case "executionSucceeded": {
			const rows = resource(s, c, "sfn", n.machine)?.details?.Executions;
			return (
				Array.isArray(rows) &&
				rows.some((e) => e.Name === n.execution && e.Status === "SUCCEEDED")
			);
		}
		case "secret":
			return resource(s, c, "secretsmanager", n.secret)?.type === "secret";
		case "parameter":
			return resource(s, c, "ssm", n.parameter)?.type === "parameter";
		case "invoke":
			return successful(
				s,
				c,
				"lambda",
				"Invoke",
				(r) =>
					r.resource_identifier === n.function ||
					r.resource_identifier === arn("lambda", `function:${n.function}`, c),
			);
		case "executionStarted": {
			const rows = resource(s, c, "sfn", n.machine)?.details?.Executions;
			return Array.isArray(rows) && rows.some((e) => e.Name === n.execution);
		}
		case "secretValue":
			return successful(s, c, "secretsmanager", "GetSecretValue", (r) => {
				const id = r.resource_identifier;
				return (
					id === n.secret ||
					(!!id && id === resource(s, c, "secretsmanager", n.secret)?.arn)
				);
			});
		case "parameterValue":
			return successful(s, c, "ssm", "GetParameter", (r) => {
				const id = r.resource_identifier?.split(":").at(-1);
				const target = r.resource_identifier;
				return (
					target === n.parameter ||
					target === arn("ssm", `parameter/${n.parameter}`, c) ||
					(!!target &&
						/^\d+$/.test(id ?? "") &&
						target.slice(0, target.lastIndexOf(":")) === n.parameter)
				);
			});
		case "iamRole":
			return !!tutorialRole(s, c);
		case "iamPolicy": {
			const policies = tutorialRole(s, c)?.details?.InlinePolicies;
			return (
				Array.isArray(policies) &&
				policies.includes(n.policy) &&
				successful(s, c, "iam", "PutRolePolicy", (_, i) => {
					const doc = json(String(i.PolicyDocument ?? ""));
					const statements = doc.Statement;
					if (!Array.isArray(statements) || statements.length !== 1)
						return false;
					const st = statements[0];
					return (
						!!st &&
						typeof st === "object" &&
						i.RoleName === n.role &&
						i.PolicyName === n.policy &&
						st.Effect === "Allow" &&
						st.Action === "s3:GetObject" &&
						st.Resource === `arn:aws:s3:::${n.bucket}/${n.key}`
					);
				})
			);
		}
		case "iamSimulation":
			return successful(s, c, "iam", "SimulatePrincipalPolicy", (r, i) => {
				if (i.PolicySourceArn !== `arn:aws:iam::${c.account}:role/${n.role}`)
					return false;
				const results = output(r).EvaluationResults;
				if (!Array.isArray(results)) {
					const rows = r.response_body.split("<EvalActionName>").slice(1);
					const target = `arn:aws:s3:::${n.bucket}/${n.key}`;
					return (
						rows.some(
							(row) =>
								row.startsWith("s3:GetObject</EvalActionName>") &&
								row.includes(
									`<EvalResourceName>${target}</EvalResourceName>`,
								) &&
								row.includes("<EvalDecision>allowed</EvalDecision>"),
						) &&
						rows.some(
							(row) =>
								row.startsWith("s3:DeleteObject</EvalActionName>") &&
								row.includes(
									`<EvalResourceName>${target}</EvalResourceName>`,
								) &&
								row.includes("<EvalDecision>implicitDeny</EvalDecision>"),
						)
					);
				}
				const target = `arn:aws:s3:::${n.bucket}/${n.key}`;
				return (
					results.some(
						(e) =>
							e.EvalActionName === "s3:GetObject" &&
							e.EvalResourceName === target &&
							e.EvalDecision === "allowed",
					) &&
					results.some(
						(e) =>
							e.EvalActionName === "s3:DeleteObject" &&
							e.EvalResourceName === target &&
							e.EvalDecision === "implicitDeny",
					)
				);
			});
		case "cognitoPool":
			return !!tutorialPool(s, c);
		// Client creation is redacted; inventory exposes only the pool's client count.
		case "cognitoClient":
			return Number(tutorialPool(s, c)?.details?.Clients ?? 0) > 0;
		case "cognitoSignup":
		case "cognitoMail": {
			const pool = tutorialPool(s, c);
			return (
				!!pool &&
				(kind === "cognitoMail" || Number(pool.details?.Users ?? 0) > 0) &&
				s.messages.some(
					(m) =>
						m.service === "cognito-idp" &&
						m.channel === "email" &&
						m.account === c.account &&
						m.region === c.region &&
						m.source === pool.arn &&
						m.to === n.email &&
						m.subject === "Your verification code" &&
						/\b\d{6}\b/.test(m.body ?? ""),
				)
			);
		}
		// Bodies stay redacted; the request log keeps only ConfirmSignUp's Username.
		case "cognitoConfirmed":
			return successful(
				s,
				c,
				"cognitoidp",
				"ConfirmSignUp",
				(r) => r.resource_identifier === n.username,
			);
		case "cfnStack":
			return resource(s, c, "cloudformation", n.stack)?.type === "stack";
		case "cfnCreated":
			return (
				resource(s, c, "cloudformation", n.stack)?.details?.Status ===
				"CREATE_COMPLETE"
			);
		case "cfnUpdated":
			return (
				resource(s, c, "cloudformation", n.stack)?.details?.Status ===
				"UPDATE_COMPLETE"
			);
		case "cfnOutputs": {
			const stack = resource(s, c, "cloudformation", n.stack);
			return (
				!!stack &&
				successful(s, c, "cloudformation", "DescribeStacks", (r, i) => {
					if (i.StackName !== n.stack && i.StackName !== stack.arn)
						return false;
					const stacks = output(r).Stacks;
					if (!Array.isArray(stacks)) {
						const values = r.response_body.match(
							/<OutputKey>QueueUrl<\/OutputKey>\s*<OutputValue>([^<]+)<\/OutputValue>/,
						);
						return (
							r.response_body.includes(`<StackName>${n.stack}</StackName>`) &&
							!!values?.[1].endsWith(`/${c.account}/${n.queue}`)
						);
					}

					return (
						Array.isArray(stacks) &&
						stacks.some(
							(st) =>
								st.StackName === n.stack &&
								Array.isArray(st.Outputs) &&
								st.Outputs.some(
									(o: { OutputKey?: string; OutputValue?: string }) =>
										o.OutputKey === "QueueUrl" &&
										o.OutputValue?.endsWith(`/${c.account}/${n.queue}`),
								),
						)
					);
				})
			);
		}
	}
}
