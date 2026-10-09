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
export type { ActionStep as SQSStep } from "./consoleActions";
export { errorText, integer, jsonObject, stringMap } from "./consoleActions";
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
