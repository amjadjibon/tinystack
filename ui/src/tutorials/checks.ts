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
	| "receive";
export const sources: Record<Check, (keyof Snapshot)[]> = {
	bucket: ["resources"],
	object: ["requests"],
	download: ["requests"],
	topic: ["resources"],
	queue: ["resources"],
	subscription: ["resources"],
	delivery: ["deliveries"],
	receive: ["requests"],
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
				const body = json(r.response_body);
				const out =
					body.output && typeof body.output === "object"
						? (body.output as Record<string, unknown>)
						: body;
				return (
					matches &&
					((Array.isArray(out.Messages) && out.Messages.length > 0) ||
						/<Message>/.test(r.response_body))
				);
			});
	}
}
