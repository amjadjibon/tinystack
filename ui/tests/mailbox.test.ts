import assert from "node:assert/strict";
import test from "node:test";
import {
	emptyMessageFilters,
	mailboxPoolHref,
	messageContent,
	messagesQuery,
} from "../src/lib/mailboxHelpers.ts";

test("mailbox filters map only supported parameters and encode phone, email and timestamps", () => {
	assert.equal(messagesQuery(emptyMessageFilters), "");
	const filters = {
		channel: "sms",
		to: "+15551234567",
		service: "cognito-idp",
		since: "2026-10-30T12:00:00+08:00",
	};
	const query = new URLSearchParams(messagesQuery(filters).slice(1));
	assert.deepEqual(Object.fromEntries(query), filters);
	assert.equal(
		new URLSearchParams(
			messagesQuery({ ...emptyMessageFilters, to: "a+b@example.com" }),
		).get("to"),
		"a+b@example.com",
	);
});
const message = {
	id: "id",
	time: "2026-10-30T00:00:00Z",
	channel: "email",
	service: "ses",
	to: "a@example.com",
};
test("rich details require the matching delivery and survive missing or truncated previews", () => {
	const delivery = {
		time: message.time,
		via: "ses",
		target: message.to,
		payload: JSON.stringify({
			messageId: "id",
			html: "<b>hi</b>",
			cc: ["b@example.com"],
		}),
		size: 50,
	};
	assert.deepEqual(messageContent(message, [delivery]), {
		messageId: "id",
		html: "<b>hi</b>",
		cc: ["b@example.com"],
	});
	assert.deepEqual(
		messageContent(message, [{ ...delivery, time: "other" }]),
		{},
	);
	assert.deepEqual(
		messageContent(message, [{ ...delivery, payload: "{truncated" }]),
		{},
	);
	assert.deepEqual(messageContent(message, []), {});
	const cognito = {
		...message,
		service: "cognito-idp",
		account: "123456789012",
		region: "us-east-1",
	};
	assert.deepEqual(
		messageContent(cognito, [
			{
				...delivery,
				via: "cognito",
				payload: JSON.stringify({
					destination: message.to,
					userPoolId: "pool",
					username: "alice",
				}),
			},
		]),
		{ userPoolId: "pool", username: "alice" },
	);
	assert.equal(
		new URL(
			mailboxPoolHref(cognito, "pool/a"),
			"http://local",
		).searchParams.get("name"),
		"pool/a",
	);
});

test("Cognito generic links use inventory names instead of pool IDs", () => {
	const message = {
		id: "id",
		time: "now",
		channel: "email",
		service: "cognito-idp",
		to: "a@example.com",
		account: "123456789012",
		region: "us-east-1",
	};
	const resources = [
		{
			service: "cognitoidp",
			type: "user-pool",
			account: message.account,
			region: message.region,
			name: "Signups",
			arn: "arn:aws:cognito-idp:us-east-1:123456789012:userpool/us-east-1_abc",
		},
	];
	const href = new URL(
		mailboxPoolHref(message, "us-east-1_abc", resources),
		"http://local",
	);
	assert.equal(href.pathname, "/services/cognitoidp/resource");
	assert.equal(href.searchParams.get("name"), "Signups");
});
