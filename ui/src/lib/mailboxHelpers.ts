export type MessageFilters = {
	channel: string;
	to: string;
	service: string;
	since: string;
};
export const emptyMessageFilters: MessageFilters = {
	channel: "",
	to: "",
	service: "",
	since: "",
};
export function messagesQuery(filters: MessageFilters) {
	const query = new URLSearchParams();
	for (const field of ["channel", "to", "service", "since"] as const)
		if (filters[field]) query.set(field, filters[field]);
	return query.size ? `?${query}` : "";
}
export type MailMessage = Omit<MessageContent, "to"> & {
	id: string;
	time: string;
	channel: string;
	service: string;
	source?: string;
	to: string;
	subject?: string;
	body?: string;
	account?: string;
	region?: string;
};
export type MessageContent = {
	from?: string;
	to?: string[];
	cc?: string[];
	bcc?: string[];
	text?: string;
	html?: string;
	raw?: string;
	userPoolId?: string;
	username?: string;
	attachments?: { name: string; size: number }[];
};
export type MessageDelivery = {
	time: string;
	via: string;
	target: string;
	payload: string;
	size: number;
};
// Messages are a flattened view. Use the matching existing delivery payload for rich details.
// Truncated or malformed payloads never masquerade as complete message content.
export function messageContent(
	message: MailMessage,
	deliveries: MessageDelivery[],
): MessageContent {
	const via = message.service === "cognito-idp" ? "cognito" : message.service;
	for (const delivery of deliveries) {
		if (delivery.time !== message.time || delivery.via !== via) continue;
		try {
			const value = JSON.parse(delivery.payload);
			if (message.service === "ses" && value.messageId === message.id)
				return value;
			if (
				message.service === "cognito-idp" &&
				(value.destination === message.to ||
					delivery.target === `${message.channel}:${message.to}`)
			)
				return { userPoolId: value.userPoolId, username: value.username };
		} catch {
			/* Delivery previews may be truncated. The flattened body is still available. */
		}
	}
	return {};
}
export function mailboxPoolHref(
	message: MailMessage,
	pool: string,
	resources: {
		service: string;
		type: string;
		name: string;
		arn: string;
		account: string;
		region: string;
	}[] = [],
) {
	const resource = resources.find(
		(r) =>
			r.service === "cognitoidp" &&
			r.type === "user-pool" &&
			r.account === message.account &&
			r.region === message.region &&
			r.arn.endsWith(`/${pool}`),
	);
	return `/services/cognitoidp?${new URLSearchParams({ id: pool, account: resource?.account ?? message.account ?? "", region: resource?.region ?? message.region ?? "" })}`;
}
