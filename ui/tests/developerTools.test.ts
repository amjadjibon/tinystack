import assert from "node:assert/strict";
import test from "node:test";
import {
	emptyFaultForm,
	faultRequest,
	faultRule,
	initPending,
	latencyValue,
} from "../src/lib/developerTools.ts";

test("fault form preserves explicit zero and builds append and exact delete requests", () => {
	const rule = faultRule({
		...emptyFaultForm,
		service: "s3",
		operation: "GetObject",
		region: "us-east-1",
		account: "123456789012",
		code: "SlowDown",
		message: "retry",
		statusCode: "503",
		probability: "0",
		latencyMs: "20",
	});
	assert.deepEqual(rule, {
		service: "s3",
		operation: "GetObject",
		region: "us-east-1",
		account: "123456789012",
		error: { code: "SlowDown", message: "retry", statusCode: 503 },
		probability: 0,
		latencyMs: 20,
	});
	assert.deepEqual(faultRequest("PATCH", [rule]), {
		path: "/_tinystack/faults",
		method: "PATCH",
		body: [rule],
	});
	assert.equal(faultRequest("DELETE", [rule]).body[0], rule);
	assert.deepEqual(faultRequest("DELETE", []).body, []);
	assert.deepEqual(faultRule({ ...emptyFaultForm, latencyMs: "3600000" }), {
		latencyMs: 3600000,
	});
	assert.deepEqual(faultRule({ ...emptyFaultForm, code: "Oops" }), {
		error: { code: "Oops" },
	});
});
test("fault form rejects server-invalid selectors, effects and rule counts", () => {
	for (const change of [
		{},
		{ service: "s*" },
		{ operation: "A B" },
		{ region: "west" },
		{ account: "123" },
		{ code: "bad code" },
		{ statusCode: "399" },
		{ statusCode: "600" },
		{ statusCode: "400.5" },
		{ probability: "NaN" },
		{ probability: "Infinity" },
		{ probability: "-0.1" },
		{ probability: "1.1" },
		{ probability: " " },
		{ latencyMs: "-1" },
		{ latencyMs: "3600001" },
		{ latencyMs: "0.5" },
		{ latencyMs: "Infinity" },
	]) {
		const form = { ...emptyFaultForm, code: "Oops", ...change };
		if (Object.keys(change).length === 0) form.code = "";
		assert.throws(() => faultRule(form), JSON.stringify(change));
	}
	assert.throws(() => faultRule({ ...emptyFaultForm, code: "Oops" }, 1024));
	assert.equal(latencyValue("0"), 0);
});
test("init polling follows running stages and pending scripts, not unused shutdown stage", () => {
	const status = {
		completed: {},
		stages: { READY: "SUCCESSFUL", SHUTDOWN: "UNKNOWN" },
		scripts: [],
	};
	assert.equal(initPending(status), false);
	assert.equal(initPending({ ...status, stages: { READY: "RUNNING" } }), true);
	assert.equal(
		initPending({
			...status,
			scripts: [
				{ stage: "READY", name: "seed", path: "seed", state: "UNKNOWN" },
			],
		}),
		true,
	);
});
