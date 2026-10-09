import { api } from "./api";

export type SQSScope = { account: string; region: string; queue?: string };
export type QueueAttributes = Record<string, string>;
export type SQSMessage = {
	MessageId: string;
	ReceiptHandle: string;
	Body: string;
	MD5OfBody: string;
	Attributes?: Record<string, string>;
	MessageAttributes?: Record<
		string,
		{ DataType: string; StringValue?: string; BinaryValue?: string }
	>;
};
export type MoveTask = {
	TaskHandle?: string;
	Status: string;
	DestinationArn?: string;
	FailureReason?: string;
	ApproximateNumberOfMessagesMoved: number;
	ApproximateNumberOfMessagesToMove: number;
	StartedTimestamp: number;
};
export type SQSStep = { label: string; run: () => Promise<unknown> };
export function sqsHref(scope: SQSScope, create = false) {
	const search = new URLSearchParams({
		account: scope.account,
		region: scope.region,
	});
	if (scope.queue) search.set("queue", scope.queue);
	if (create) search.set("create", "true");
	return `/services/sqs?${search}`;
}
// SQS accepts path-only queue URLs for in-process calls. The regional scope
// comes from the envelope; the queue path carries the owning account.
export function queuePath(scope: SQSScope) {
	return `/${scope.account}/${scope.queue}`;
}
export function sqsCall<T = Record<string, unknown>>(
	scope: SQSScope,
	operation: string,
	input: Record<string, unknown> = {},
) {
	return api.action<T>("sqs", operation, scope.account, input, scope.region);
}
export function queueCall<T = Record<string, unknown>>(
	scope: SQSScope,
	operation: string,
	input: Record<string, unknown> = {},
) {
	return sqsCall<T>(scope, operation, { QueueUrl: queuePath(scope), ...input });
}
export function errorText(error: unknown) {
	return error instanceof Error ? error.message : String(error);
}
export function jsonObject(
	text: string,
	label: string,
): Record<string, unknown> {
	let value: unknown;
	try {
		value = JSON.parse(text);
	} catch {
		throw new Error(`${label} must be valid JSON.`);
	}
	if (!value || typeof value !== "object" || Array.isArray(value))
		throw new Error(`${label} must be a JSON object.`);
	return value as Record<string, unknown>;
}
export function stringMap(text: string, label: string): Record<string, string> {
	const value = jsonObject(text, label);
	if (Object.values(value).some((v) => typeof v !== "string"))
		throw new Error(`${label} values must be strings.`);
	return value as Record<string, string>;
}
export function integer(text: string, label: string, min: number, max: number) {
	if (
		!/^\d+$/.test(text) ||
		!Number.isSafeInteger(Number(text)) ||
		Number(text) < min ||
		Number(text) > max
	)
		throw new Error(`${label} must be an integer from ${min} to ${max}.`);
	return Number(text);
}
export const queueNumbers = [
	{
		key: "VisibilityTimeout",
		label: "Visibility timeout",
		min: 0,
		max: 43200,
		value: "30",
		unit: "seconds",
	},
	{
		key: "MessageRetentionPeriod",
		label: "Message retention period",
		min: 60,
		max: 1209600,
		value: "345600",
		unit: "seconds",
	},
	{
		key: "DelaySeconds",
		label: "Delivery delay",
		min: 0,
		max: 900,
		value: "0",
		unit: "seconds",
	},
	{
		key: "MaximumMessageSize",
		label: "Maximum message size",
		min: 1024,
		max: 1048576,
		value: "262144",
		unit: "bytes",
	},
	{
		key: "ReceiveMessageWaitTimeSeconds",
		label: "Receive message wait time",
		min: 0,
		max: 20,
		value: "0",
		unit: "seconds",
	},
] as const;
export function redriveTarget(attributes: Record<string, unknown> = {}) {
	try {
		return String(
			JSON.parse(String(attributes.RedrivePolicy ?? "{}"))
				.deadLetterTargetArn ?? "",
		);
	} catch {
		return "";
	}
}
