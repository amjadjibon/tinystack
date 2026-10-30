import { adminRequest } from "./adminApi";
import type { Delivery } from "./api";
import {
	type MailMessage,
	type MessageFilters,
	messagesQuery,
} from "./mailboxHelpers";
export const mailbox = {
	list: async (filters: MessageFilters) =>
		(
			await adminRequest<{ messages: MailMessage[] }>(
				`/_tinystack/messages${messagesQuery(filters)}`,
			)
		).messages,
	deliveries: async () =>
		(await adminRequest<{ deliveries: Delivery[] }>("/_tinystack/deliveries"))
			.deliveries,
	clear: () => adminRequest<void>("/_tinystack/messages", "DELETE"),
};
