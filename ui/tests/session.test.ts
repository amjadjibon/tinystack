import assert from "node:assert/strict";
import { test } from "node:test";
import {
	connectConsoleSession,
	consoleActionFetch,
	disconnectConsoleSession,
} from "../src/lib/session.ts";

test("console session signing, account bounds and credential lifetime", async () => {
	const originalFetch = globalThis.fetch;
	const originalWindow = Object.getOwnPropertyDescriptor(globalThis, "window");
	Object.defineProperty(globalThis, "window", {
		configurable: true,
		value: {
			location: new URL("http://localhost:4567"),
			crypto: globalThis.crypto,
		},
	});
	const account = "000000000000";
	const secret = "memory-only-secret";
	const calls: { body: string; headers: Headers }[] = [];
	const lastCall = () => {
		const call = calls.at(-1);
		assert.ok(call, "expected a console action request");
		return call;
	};
	let failure: string | undefined;
	let arn = `arn:aws:iam::${account}:root`;
	let requiresSignature = true;
	let cancel: AbortController | undefined;
	globalThis.fetch = async (_url, init) => {
		if (!init?.method) {
			return Response.json({
				requires_signature: requiresSignature,
				region: "us-east-1",
				signing_names: { sts: "sts", iam: "iam", cloudwatch: "monitoring" },
			});
		}
		const body = String(init.body);
		calls.push({ body, headers: new Headers(init.headers) });
		if (JSON.parse(body).operation === "GetCallerIdentity") {
			cancel?.abort();
			return Response.json({ output: { Account: account, Arn: arn } });
		}
		return failure
			? Response.json(
					{ code: failure, message: "test failure" },
					{ status: 400 },
				)
			: Response.json({ output: {} });
	};
	const credentials = { accessKeyId: "local-root", secretAccessKey: secret };
	const action = JSON.stringify({
		service: "cloudwatch",
		operation: "PutMetricData",
		account,
		input: {},
	});
	const connect = () =>
		connectConsoleSession(credentials, account, new AbortController().signal);
	try {
		await connect();
		const handshake = lastCall();
		assert.match(
			handshake.headers.get("authorization") ?? "",
			/\/sts\/aws4_request/,
		);
		assert.ok(!handshake.body.includes(secret));
		await consoleActionFetch(action, "cloudwatch", account, "us-west-2");
		assert.match(
			lastCall().headers.get("authorization") ?? "",
			/\/us-west-2\/monitoring\/aws4_request/,
		);
		assert.equal(lastCall().headers.has("host"), false);
		assert.ok(!lastCall().body.includes(secret));
		const count = calls.length;
		await assert.rejects(
			consoleActionFetch(action, "cloudwatch", "111111111111"),
			/root session is for account/,
		);
		assert.equal(
			calls.length,
			count,
			"account mismatch must not send a request",
		);
		failure = "ValidationException";
		await consoleActionFetch(action, "cloudwatch", account);
		failure = undefined;
		await consoleActionFetch(action, "cloudwatch", account);
		assert.ok(
			lastCall().headers.has("authorization"),
			"service errors must retain the session",
		);
		for (const code of [
			"ExpiredTokenException",
			"UnrecognizedClientException",
			"InvalidSignatureException",
		]) {
			await connect();
			failure = code;
			await consoleActionFetch(action, "cloudwatch", account);
			failure = undefined;
			await consoleActionFetch(action, "cloudwatch", account);
			assert.equal(lastCall().headers.has("authorization"), false, code);
		}
		await connect();
		disconnectConsoleSession();
		await consoleActionFetch(action, "cloudwatch", account);
		assert.equal(lastCall().headers.has("authorization"), false);
		arn = `arn:aws:iam::${account}:user/alice`;
		await assert.rejects(connect(), /root credentials/);
		await consoleActionFetch(action, "cloudwatch", account);
		assert.equal(
			lastCall().headers.has("authorization"),
			false,
			"non-root cannot activate a session",
		);
		arn = `arn:aws:iam::${account}:root`;
		cancel = new AbortController();
		await assert.rejects(
			connectConsoleSession(credentials, account, cancel.signal),
			/cancelled/,
		);
		await consoleActionFetch(action, "cloudwatch", account);
		assert.equal(
			lastCall().headers.has("authorization"),
			false,
			"cancelled connection must not activate",
		);
		cancel = undefined;
		requiresSignature = false;
		await assert.rejects(connect(), /Signature verification is off/);
	} finally {
		disconnectConsoleSession();
		globalThis.fetch = originalFetch;
		if (originalWindow)
			Object.defineProperty(globalThis, "window", originalWindow);
		else Reflect.deleteProperty(globalThis, "window");
	}
});
