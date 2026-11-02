import assert from "node:assert/strict";
import test from "node:test";
import {
	addressList,
	createConfigSetInput,
	createDestinationInput,
	createTemplateInput,
	destinationForm,
	destinationKind,
	emptyConfigSet,
	emptyDestination,
	emptySend,
	parseTemplateData,
	quotaText,
	senderOptions,
	sendInput,
	sesMessages,
	tagList,
	tagMap,
	updateTemplateInput,
	validateIdentity,
} from "../src/lib/sesv2Helpers.ts";

test("identity names are an address or a domain", () => {
	assert.equal(validateIdentity("a@example.com"), "");
	assert.equal(validateIdentity("example.com"), "");
	assert.match(validateIdentity(""), /Enter/);
	assert.match(validateIdentity("a@b"), /not a valid email/);
	assert.match(validateIdentity("nodots"), /not a valid domain/);
});

test("recipient lists split on commas, semicolons and whitespace", () => {
	assert.deepEqual(addressList("a@x.io, b@x.io;c@x.io\n d@x.io"), [
		"a@x.io",
		"b@x.io",
		"c@x.io",
		"d@x.io",
	]);
	assert.deepEqual(addressList("  "), []);
});

test("template data must be a JSON object", () => {
	assert.equal(parseTemplateData(""), "{}");
	assert.equal(parseTemplateData('{ "a": 1 }'), '{"a":1}');
	assert.throws(() => parseTemplateData("{"), /valid JSON/);
	assert.throws(() => parseTemplateData("[1]"), /JSON object/);
});

test("template input needs a name, a subject and a body", () => {
	const form = {
		name: "welcome",
		subject: "Hi {{n}}",
		text: "Hello",
		html: "",
	};
	assert.deepEqual(createTemplateInput(form), {
		TemplateName: "welcome",
		TemplateContent: { Subject: "Hi {{n}}", Text: "Hello" },
	});
	assert.deepEqual(
		updateTemplateInput({ ...form, text: "", html: "<p>x</p>" }),
		{
			TemplateName: "welcome",
			TemplateContent: { Subject: "Hi {{n}}", Html: "<p>x</p>" },
		},
	);
	assert.throws(
		() => createTemplateInput({ ...form, name: "no spaces" }),
		/letters/,
	);
	assert.throws(
		() => createTemplateInput({ ...form, subject: " " }),
		/subject/,
	);
	assert.throws(() => createTemplateInput({ ...form, text: "" }), /body/);
});

test("simple sends build content and destination", () => {
	const input = sendInput({
		...emptySend,
		from: "me@example.com",
		to: "a@x.io, b@x.io",
		cc: "c@x.io",
		configurationSet: "set",
		subject: "Hi",
		text: "Body",
	});
	assert.deepEqual(input, {
		FromEmailAddress: "me@example.com",
		Destination: { ToAddresses: ["a@x.io", "b@x.io"], CcAddresses: ["c@x.io"] },
		ConfigurationSetName: "set",
		Content: {
			Simple: {
				Subject: { Data: "Hi", Charset: "UTF-8" },
				Body: { Text: { Data: "Body", Charset: "UTF-8" } },
			},
		},
	});
});

test("template sends carry compact data and reject bad input", () => {
	const form = {
		...emptySend,
		from: "me@example.com",
		to: "a@x.io",
		mode: "template" as const,
		template: "welcome",
		data: '{ "n": "Ada" }',
	};
	assert.deepEqual(sendInput(form).Content, {
		Template: { TemplateName: "welcome", TemplateData: '{"n":"Ada"}' },
	});
	assert.throws(() => sendInput({ ...form, from: "" }), /sender/);
	assert.throws(() => sendInput({ ...form, to: "" }), /recipient/);
	assert.throws(() => sendInput({ ...form, to: "bad" }), /not a valid/);
	assert.throws(() => sendInput({ ...form, template: "" }), /template/);
	assert.throws(
		() => sendInput({ ...form, to: Array(51).fill("a@x.io").join(",") }),
		/at most 50/,
	);
	assert.throws(
		() => sendInput({ ...emptySend, from: "a@x.io", to: "b@x.io" }),
		/subject/,
	);
});

test("configuration set input", () => {
	assert.deepEqual(
		createConfigSetInput({ ...emptyConfigSet, name: "set_1" }, { a: "b" }),
		{
			ConfigurationSetName: "set_1",
			SendingOptions: { SendingEnabled: true },
			ReputationOptions: { ReputationMetricsEnabled: false },
			DeliveryOptions: { TlsPolicy: "OPTIONAL" },
			Tags: [{ Key: "a", Value: "b" }],
		},
	);
	assert.throws(() => createConfigSetInput(emptyConfigSet), /letters/);
});

test("event destinations name exactly one target", () => {
	const sns = {
		...emptyDestination,
		name: "to-sns",
		topicArn: "arn:aws:sns:us-east-1:123456789012:events",
	};
	assert.deepEqual(createDestinationInput("set", sns), {
		ConfigurationSetName: "set",
		EventDestinationName: "to-sns",
		EventDestination: {
			Enabled: true,
			MatchingEventTypes: ["SEND"],
			SnsDestination: { TopicArn: sns.topicArn },
		},
	});
	assert.throws(
		() => createDestinationInput("set", { ...sns, topicArn: "topic" }),
		/ARN/,
	);
	assert.throws(
		() => createDestinationInput("set", { ...sns, events: [] }),
		/event type/,
	);
	assert.throws(
		() => createDestinationInput("set", { ...sns, name: "" }),
		/name/,
	);
	const stream = {
		...sns,
		kind: "firehose" as const,
		streamArn: "arn:aws:firehose:us-east-1:123456789012:deliverystream/s",
		roleArn: "arn:aws:iam::123456789012:role/r",
	};
	const definition = createDestinationInput("set", stream).EventDestination;
	assert.ok("KinesisFirehoseDestination" in definition);
	assert.ok(!("SnsDestination" in definition));
});

test("a stored destination fills the form again", () => {
	const stored = {
		Name: "bus",
		Enabled: false,
		MatchingEventTypes: ["BOUNCE"],
		EventBridgeDestination: {
			EventBusArn: "arn:aws:events:us-east-1:123456789012:event-bus/b",
		},
	};
	assert.equal(destinationKind(stored), "eventbridge");
	const form = destinationForm(stored);
	assert.equal(form.kind, "eventbridge");
	assert.equal(form.enabled, false);
	assert.equal(form.busArn, stored.EventBridgeDestination.EventBusArn);
});

test("tags convert both ways", () => {
	assert.deepEqual(tagMap([{ Key: "a", Value: "1" }, { Key: "b" }]), {
		a: "1",
		b: "",
	});
	assert.deepEqual(tagList({ a: "1" }), [{ Key: "a", Value: "1" }]);
});

test("sent messages are scoped to the account and Region, newest first", () => {
	const message = (id: string, time: string, extra = {}) => ({
		id,
		time,
		service: "ses",
		to: "a@x.io",
		account: "111111111111",
		region: "us-east-1",
		...extra,
	});
	const out = sesMessages(
		[
			message("old", "2026-01-01T00:00:00Z"),
			message("new", "2026-01-02T00:00:00Z"),
			message("other-region", "2026-01-03T00:00:00Z", { region: "eu-west-1" }),
			message("other-account", "2026-01-03T00:00:00Z", {
				account: "222222222222",
			}),
			message("sns", "2026-01-03T00:00:00Z", { service: "sns" }),
		],
		"111111111111",
		"us-east-1",
	);
	assert.deepEqual(
		out.map((m) => m.id),
		["new", "old"],
	);
});

test("senders and quotas read well", () => {
	assert.deepEqual(
		senderOptions([
			{ IdentityName: "a@x.io" },
			{ IdentityName: "x.io" },
			{},
		]).map((o) => o.value),
		["a@x.io", "no-reply@x.io"],
	);
	assert.match(
		quotaText({ SentLast24Hours: 2, Max24HourSend: 50000, MaxSendRate: 14 }),
		/2 of 50000/,
	);
	assert.equal(quotaText(), "-");
});
