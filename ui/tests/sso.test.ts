import assert from "node:assert/strict";
import { test } from "node:test";
import {
	adminFetch,
	collectSSOSession,
	consoleActionFetch,
	signOutSSO,
} from "../src/lib/session.ts";

test("SSO session handoff, signing and sign-out keep credentials in memory", async () => {
	const originalFetch = globalThis.fetch;
	const originalWindow = Object.getOwnPropertyDescriptor(globalThis, "window");
	const assigned: string[] = [];
	Object.defineProperty(globalThis, "window", {
		configurable: true,
		value: {
			location: {
				...new URL("http://localhost:4567"),
				origin: "http://localhost:4567",
				assign: (url: string) => assigned.push(url),
			},
			crypto: globalThis.crypto,
		},
	});
	const account = "111111111111";
	const secret = "sso-session-secret";
	const requests: { url: string; init?: RequestInit }[] = [];
	let waiting = true;
	let expiration = new Date(Date.now() + 3_600_000).toISOString();
	globalThis.fetch = async (url, init) => {
		const target = String(url);
		requests.push({ url: target, init });
		if (target.endsWith("/auth/session")) {
			if (!waiting) return Response.json({ code: "NoSignIn" }, { status: 401 });
			waiting = false;
			return Response.json({
				accessKeyId: "ASIAEXAMPLE",
				secretAccessKey: secret,
				sessionToken: "session-token",
				expiration,
				account,
				arn: `arn:aws:sts::${account}:assumed-role/Developer/dev@example.com`,
				role: `arn:aws:iam::${account}:role/Developer`,
				name: "dev@example.com",
				admin: false,
			});
		}
		if (target.endsWith("/auth/logout")) {
			return Response.json({ end_session_url: "https://idp.example/logout" });
		}
		if (!init?.method) {
			return Response.json({
				region: "us-east-1",
				signing_names: { sts: "sts", s3: "s3" },
				sso: { login_url: "/_tinystack/auth/login" },
			});
		}
		return Response.json({ output: {} });
	};
	const authorization = (call?: { init?: RequestInit }) =>
		new Headers(call?.init?.headers).get("authorization") ?? "";
	const action = JSON.stringify({
		service: "s3",
		operation: "ListBuckets",
		account,
		input: {},
	});
	try {
		assert.equal(await collectSSOSession(), true);
		assert.equal(await collectSSOSession(), false, "a handoff is used once");
		await consoleActionFetch(action, "s3", account, "us-east-1");
		const call = requests.at(-1);
		assert.match(authorization(call), /\/us-east-1\/s3\/aws4_request/);
		assert.equal(
			new Headers(call?.init?.headers).get("x-amz-security-token"),
			"session-token",
		);
		assert.ok(!String(call?.init?.body).includes(secret));
		await assert.rejects(
			consoleActionFetch(action, "s3", "000000000000"),
			/SSO session is for account/,
		);
		await adminFetch("/_tinystack/requests");
		assert.match(authorization(requests.at(-1)), /\/tinystack\/aws4_request/);
		assert.equal(await signOutSSO(), "https://idp.example/logout");
		await consoleActionFetch(action, "s3", account);
		assert.equal(authorization(requests.at(-1)), "", "signed out");
		waiting = true;
		expiration = new Date(Date.now() - 1000).toISOString();
		await assert.rejects(collectSSOSession(), /not valid/);
	} finally {
		await signOutSSO();
		globalThis.fetch = originalFetch;
		if (originalWindow)
			Object.defineProperty(globalThis, "window", originalWindow);
		else Reflect.deleteProperty(globalThis, "window");
	}
	assert.deepEqual(assigned, []);
});
