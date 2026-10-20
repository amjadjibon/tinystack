import assert from "node:assert/strict";
import { test } from "node:test";
import type { RequestRecord } from "../src/lib/api.ts";
import {
	causedDeliveries,
	deliveryContext,
	isDenial,
} from "../src/lib/requestLog.ts";

const request: RequestRecord = {
	request_id: "parent",
	method: "POST",
	path: "/",
	time: "",
	account: "",
	region: "",
	request_body: "",
	response_body: "",
	status: 200,
	request_size: 0,
	response_size: 0,
	duration_ms: 0,
};
const delivery: RequestRecord = {
	...request,
	request_id: "child",
	method: "DELIVER",
	request_body: JSON.stringify({
		principal: "sns.amazonaws.com",
		source_arn: "source",
		target: "target",
		caused_by: "parent",
	}),
	authorization: {
		mode: "soft",
		allowed: false,
		principal: "sns.amazonaws.com",
		message: "would deny",
	},
};

test("delivery context distinguishes HTTP bodies, role trust failures and malformed records", () => {
	assert.equal(
		deliveryContext({ ...request, request_body: delivery.request_body }),
		undefined,
	);
	assert.equal(deliveryContext(delivery)?.principal, "sns.amazonaws.com");
	assert.deepEqual(
		deliveryContext({
			...delivery,
			request_body: JSON.stringify({
				role: "role",
				service: "scheduler.amazonaws.com",
				source_arn: "schedule",
				unexpected: 1,
			}),
		}),
		{
			role: "role",
			service: "scheduler.amazonaws.com",
			source_arn: "schedule",
		},
	);
	for (const request_body of ["{", "null", "[]", "42"]) {
		assert.deepEqual(deliveryContext({ ...delivery, request_body }), {});
	}
});

test("denial filtering includes soft denials with status 200 and enforced trust failures", () => {
	assert.ok(delivery.authorization);
	assert.equal(isDenial(delivery), true);
	assert.equal(
		isDenial({
			...delivery,
			authorization: { ...delivery.authorization, mode: "enforce" },
			status: 403,
		}),
		true,
	);
	assert.equal(
		isDenial({
			...delivery,
			authorization: { ...delivery.authorization, allowed: true },
		}),
		false,
	);
	assert.equal(isDenial(request), false);
	assert.equal(isDenial({ ...request, status: 403 }), true);
	assert.equal(isDenial({ ...request, status: 500 }), false);
});

test("causal links use all retained records regardless of display filters", () => {
	const background = {
		...delivery,
		request_id: "background",
		request_body: "{}",
	};
	assert.deepEqual(
		causedDeliveries([request, delivery, background], "parent"),
		[delivery],
	);
	assert.deepEqual(causedDeliveries([delivery], "child"), []);
	assert.equal(deliveryContext(background)?.caused_by, undefined);
});
