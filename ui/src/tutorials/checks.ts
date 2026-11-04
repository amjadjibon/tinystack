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
	| "event";
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
			r.region !== c.region ||
			r.service !== service ||
			r.operation !== operation ||
			r.status < 200 ||
			r.status >= 300 ||
			r.error_code
		)
			return false;
		const body = json(r.request_body);
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
	}
}
