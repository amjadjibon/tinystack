import { useSyncExternalStore } from "react";

export type ConsoleCredentials = {
	accessKeyId: string;
	secretAccessKey: string;
	sessionToken?: string;
};
export type ConsoleSession = {
	account: string;
	arn: string;
	// Set for a person who signed in through the identity provider.
	sso?: SSOPerson;
};
export type SSOPerson = {
	name: string;
	role: string;
	admin: boolean;
	expiresAt: number;
};
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
let generation = 0;
const listeners = new Set<() => void>();

// An SSO session starts the sign-in again shortly before its credentials expire.
const renewBefore = 60_000;
let renewal: ReturnType<typeof setTimeout> | undefined;
let loginURL = "/_tinystack/auth/login";

function publish(session?: SignedSession) {
	generation++;
	clearTimeout(renewal);
	renewal = undefined;
	active = session;
	snapshot = session?.info ?? null;
	const sso = session?.info.sso;
	if (sso) {
		const remaining = sso.expiresAt - Date.now();
		renewal = setTimeout(
			() => window.location.assign(loginURL),
			Math.max(remaining - renewBefore, remaining / 2),
		);
	}
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
	request: { path: string; method: string } = {
		path: "/_tinystack/actions",
		method: "POST",
	},
) {
	const [{ SignatureV4 }, { HttpRequest }, { Sha256 }] = await Promise.all([
		import("@smithy/signature-v4"),
		import("@smithy/protocol-http"),
		import("@aws-crypto/sha256-browser"),
	]);
	const url = new URL(request.path, window.location.origin);
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
			method: request.method,
			path: url.pathname,
			headers: {
				host: url.host,
				...(body ? { "content-type": "application/json" } : {}),
			},
			body: body || undefined,
		}),
	);
	// Browsers supply Host themselves; its signed value remains the same origin.
	const headers = { ...signed.headers };
	delete headers.host;
	return fetch(url, {
		method: request.method,
		headers,
		body: body || undefined,
		signal,
	});
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

// disconnectIfExpired ends the session when TinyStack refused its credentials.
async function disconnectIfExpired(response: Response, session: SignedSession) {
	if (
		(response.status !== 400 && response.status !== 403) ||
		active !== session
	)
		return;
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
			`This ${session.info.sso ? "SSO" : "root"} session is for account ${session.info.account}. Connect credentials for ${account}.`,
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
	await disconnectIfExpired(response, session);
	return response;
}

// adminFetch calls one of TinyStack's own endpoints, such as the request
// inspector. With SSO configured these accept only signed requests from root
// credentials or a signed-in person, so a connected session signs them.
export async function adminFetch(path: string, method = "GET", body?: unknown) {
	const session = active;
	const payload = body === undefined ? "" : JSON.stringify(body);
	if (!session)
		return fetch(path, {
			method,
			...(payload
				? { headers: { "Content-Type": "application/json" }, body: payload }
				: {}),
		});
	return signedFetch(
		payload,
		"tinystack",
		session.region,
		session.credentials,
		undefined,
		{ path, method },
	);
}

// shellFetch sends one CloudShell request. With a connected session it is
// signed for TinyStack's own endpoints, since the service a command calls is
// only known to the server; the server then runs the command as that person.
export async function shellFetch(body: string, signal?: AbortSignal) {
	const session = active;
	const path = "/_tinystack/shell";
	if (!session) {
		return fetch(path, {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body,
			signal,
		});
	}
	const response = await signedFetch(
		body,
		"tinystack",
		session.region,
		session.credentials,
		signal,
		{ path, method: "POST" },
	);
	await disconnectIfExpired(response, session);
	return response;
}

type SSOHandoff = {
	accessKeyId: string;
	secretAccessKey: string;
	sessionToken: string;
	expiration: string;
	account: string;
	arn: string;
	role: string;
	name: string;
	admin: boolean;
};

// collectSSOSession takes the credentials of the person signed in with this
// browser. The server hands them over in the body of a same-origin response
// while its HttpOnly session cookie lasts, and they stay in memory like a root
// session's. It reports false when nobody is signed in.
export async function collectSSOSession(signal?: AbortSignal) {
	const started = generation;
	if (typeof window === "undefined")
		throw new Error("Console sessions are browser-only.");
	const response = await fetch("/_tinystack/auth/session", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: "{}",
		signal,
	});
	if (response.status === 401) return false;
	if (!response.ok) throw new Error("Cannot collect the SSO session.");
	const handoff = (await response.json()) as SSOHandoff;
	const expiresAt = Date.parse(handoff.expiration);
	if (
		!/^\d{12}$/.test(handoff.account) ||
		!handoff.accessKeyId ||
		!handoff.secretAccessKey ||
		!handoff.sessionToken ||
		!(expiresAt > Date.now())
	) {
		throw new Error("The SSO session is not valid.");
	}
	const capabilitiesResponse = await fetch("/_tinystack/actions", { signal });
	if (!capabilitiesResponse.ok)
		throw new Error("Cannot load console session capabilities.");
	const capabilities = (await capabilitiesResponse.json()) as {
		region: string;
		signing_names?: Record<string, string>;
		sso?: { login_url: string };
	};
	if (!capabilities.signing_names?.sts)
		throw new Error("Update TinyStack to enable signed console sessions.");
	if (signal?.aborted)
		throw new DOMException("Connection cancelled", "AbortError");
	if (generation !== started) return false;
	loginURL = capabilities.sso?.login_url ?? loginURL;
	publish({
		credentials: {
			accessKeyId: handoff.accessKeyId,
			secretAccessKey: handoff.secretAccessKey,
			sessionToken: handoff.sessionToken,
		},
		info: {
			account: handoff.account,
			arn: handoff.arn,
			sso: {
				name: handoff.name,
				role: handoff.role,
				admin: handoff.admin,
				expiresAt,
			},
		},
		region: capabilities.region,
		signingNames: capabilities.signing_names,
	});
	return true;
}

// signedOut is set when a person signs out, for the sign-in page to confirm it:
// the identity provider's end-session address, or "" when it has none.
let signedOut: string | undefined;

export function lastSignOut() {
	return signedOut;
}

// signOutSSO ends the console session. The identity provider's end-session
// address is returned when it has one, for the person to follow.
export async function signOutSSO() {
	let endSession: string | undefined;
	disconnectConsoleSession();
	try {
		const response = await fetch("/_tinystack/auth/logout", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: "{}",
		});
		if (response.ok)
			endSession = ((await response.json()) as { end_session_url?: string })
				.end_session_url;
	} finally {
		signedOut = endSession ?? "";
		disconnectConsoleSession();
	}
	return endSession;
}
