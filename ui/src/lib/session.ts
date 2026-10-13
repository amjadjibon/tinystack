import { useSyncExternalStore } from "react";

export type ConsoleCredentials = {
	accessKeyId: string;
	secretAccessKey: string;
	sessionToken?: string;
};
export type ConsoleSession = { account: string; arn: string };
type SignedSession = {
	credentials: ConsoleCredentials;
	info: ConsoleSession;
	region: string;
	signingNames: Record<string, string>;
};

// Only browser event handlers establish a session. Secrets never enter the
// public snapshot, SSR state, browser storage or the console action body.
let active: SignedSession | undefined;
let snapshot: ConsoleSession | null = null;
const listeners = new Set<() => void>();

function publish(session?: SignedSession) {
	active = session;
	snapshot = session?.info ?? null;
	for (const listener of listeners) listener();
}

export function disconnectConsoleSession() {
	publish();
}

export function useConsoleSession() {
	return useSyncExternalStore(
		(listener) => {
			listeners.add(listener);
			return () => listeners.delete(listener);
		},
		() => snapshot,
		() => null,
	);
}

async function signedFetch(
	body: string,
	service: string,
	region: string,
	credentials: ConsoleCredentials,
	signal?: AbortSignal,
) {
	const [{ SignatureV4 }, { HttpRequest }, { Sha256 }] = await Promise.all([
		import("@smithy/signature-v4"),
		import("@smithy/protocol-http"),
		import("@aws-crypto/sha256-browser"),
	]);
	const url = new URL("/_tinystack/actions", window.location.origin);
	const signed = await new SignatureV4({
		credentials,
		service,
		region,
		sha256: Sha256,
	}).sign(
		new HttpRequest({
			protocol: url.protocol,
			hostname: url.hostname,
			port: url.port ? Number(url.port) : undefined,
			method: "POST",
			path: url.pathname,
			headers: { host: url.host, "content-type": "application/json" },
			body,
		}),
	);
	// Browsers supply Host themselves; its signed value remains the same origin.
	const headers = { ...signed.headers };
	delete headers.host;
	return fetch(url, { method: "POST", headers, body, signal });
}

export async function connectConsoleSession(
	credentials: ConsoleCredentials,
	account: string,
	signal: AbortSignal,
) {
	if (typeof window === "undefined")
		throw new Error("Console sessions are browser-only.");
	if (
		!/^\d{12}$/.test(account) ||
		!credentials.accessKeyId ||
		!credentials.secretAccessKey
	) {
		throw new Error(
			"Enter a 12-digit account ID, access key ID and secret access key.",
		);
	}
	const capabilitiesResponse = await fetch("/_tinystack/actions", { signal });
	if (!capabilitiesResponse.ok)
		throw new Error("Cannot load console session capabilities.");
	const capabilities = (await capabilitiesResponse.json()) as {
		requires_signature: boolean;
		region: string;
		signing_names?: Record<string, string>;
	};
	if (!capabilities.requires_signature) {
		throw new Error(
			"Signature verification is off. Parse mode already supports unsigned console forms.",
		);
	}
	const signingNames = capabilities.signing_names;
	if (!signingNames?.sts)
		throw new Error("Update TinyStack to enable signed console sessions.");
	const body = JSON.stringify({
		service: "sts",
		operation: "GetCallerIdentity",
		account,
		region: capabilities.region,
		input: {},
	});
	const response = await signedFetch(
		body,
		signingNames.sts,
		capabilities.region,
		credentials,
		signal,
	);
	const result = await response.json();
	if (!response.ok) throw new Error(`${result.code}: ${result.message}`);
	if (
		result.output?.Account !== account ||
		result.output?.Arn !== `arn:aws:iam::${account}:root`
	) {
		throw new Error(
			"Use account root credentials for the administrative console.",
		);
	}
	if (signal.aborted)
		throw new DOMException("Connection cancelled", "AbortError");
	publish({
		credentials: { ...credentials },
		info: { account, arn: result.output.Arn },
		region: capabilities.region,
		signingNames,
	});
}

export async function consoleActionFetch(
	body: string,
	service: string,
	account: string,
	region?: string,
) {
	const session = active;
	if (!session) {
		return fetch("/_tinystack/actions", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body,
		});
	}
	if (session.info.account !== account) {
		throw new Error(
			`This root session is for account ${session.info.account}. Connect credentials for ${account}.`,
		);
	}
	const signingName = session.signingNames[service];
	if (!signingName)
		throw new Error("This service does not support signed console actions.");
	const response = await signedFetch(
		body,
		signingName,
		region || session.region,
		session.credentials,
	);
	if (
		(response.status === 400 || response.status === 403) &&
		active === session
	) {
		const failure = await response
			.clone()
			.json()
			.catch(() => null);
		if (
			[
				"ExpiredToken",
				"InvalidAccessKeyId",
				"InvalidClientTokenId",
				"SignatureDoesNotMatch",
				"UnrecognizedClientException",
				"InvalidSignatureException",
				"ExpiredTokenException",
			].includes(failure?.code)
		) {
			disconnectConsoleSession();
		}
	}
	return response;
}
