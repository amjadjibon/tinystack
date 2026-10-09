import { api, type Resource } from "./api";
export type SNSScope = { account: string; region: string; topic?: string };
export type SNSSubscription = {
	SubscriptionArn: string;
	TopicArn: string;
	Protocol: string;
	Endpoint: string;
	RawMessageDelivery: boolean;
	PendingConfirmation: boolean;
	FilterPolicy: string;
	FilterPolicyScope: string;
};
export function snsHref(
	scope: SNSScope,
	subscription = "",
	section = "topics",
	create = false,
) {
	const search = new URLSearchParams({
		account: scope.account,
		region: scope.region,
	});
	if (scope.topic) search.set("topic", scope.topic);
	if (subscription) search.set("subscription", subscription);
	if (section !== "topics") search.set("section", section);
	if (create) search.set("create", "true");
	return `/services/sns?${search}`;
}
export function topicArn(scope: SNSScope) {
	return `arn:aws:sns:${scope.region}:${scope.account}:${scope.topic}`;
}
export function snsCall<T = Record<string, unknown>>(
	scope: SNSScope,
	operation: string,
	input: Record<string, unknown> = {},
) {
	return api.action<T>("sns", operation, scope.account, input, scope.region);
}
export function topicCall<T = Record<string, unknown>>(
	scope: SNSScope,
	operation: string,
	input: Record<string, unknown> = {},
) {
	return snsCall<T>(scope, operation, { TopicArn: topicArn(scope), ...input });
}
export function topicSubscriptions(topic: Resource): SNSSubscription[] {
	const rows = topic.details?.Subscriptions;
	if (!Array.isArray(rows)) return [];
	return rows.map((s) => ({
		...s,
		TopicArn: topic.arn,
		FilterPolicy: s.FilterPolicy ?? "",
		FilterPolicyScope: s.FilterPolicyScope || "MessageAttributes",
		PendingConfirmation: s.PendingConfirmation === true,
		RawMessageDelivery: s.RawMessageDelivery === true,
	}));
}
export function subscriptionScope(subscription: SNSSubscription): SNSScope {
	const [, , , region, account, topic] = subscription.TopicArn.split(":");
	return { account, region, topic };
}
export function snsTags(tags: Record<string, string>) {
	return Object.entries(tags).map(([Key, Value]) => ({ Key, Value }));
}
