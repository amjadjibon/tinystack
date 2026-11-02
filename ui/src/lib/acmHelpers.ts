// Pure logic for the Certificate Manager page; no imports so Node can test it directly.

export const certificateStatuses = [
	"PENDING_VALIDATION",
	"ISSUED",
	"INACTIVE",
	"EXPIRED",
	"VALIDATION_TIMED_OUT",
	"REVOKED",
	"FAILED",
];
// ListCertificates returns only RSA_2048 certificates unless key types are named.
export const allKeyTypes = [
	"RSA_1024",
	"RSA_2048",
	"RSA_3072",
	"RSA_4096",
	"EC_prime256v1",
	"EC_secp384r1",
	"EC_secp521r1",
];
export const requestKeyAlgorithms = [
	"RSA_2048",
	"EC_prime256v1",
	"EC_secp384r1",
];
export const validationMethods = ["DNS", "EMAIL"];
export const passphraseMin = 4;
export const passphraseMax = 128;

export type ResourceRecord = { Name: string; Type: string; Value: string };
export type DomainValidation = {
	DomainName?: string;
	ValidationDomain?: string;
	ValidationStatus?: string;
	ValidationMethod?: string;
	ValidationEmails?: string[];
	ResourceRecord?: ResourceRecord;
};

export function certificateType(type: string | undefined) {
	return type === "IMPORTED"
		? "Imported"
		: type === "AMAZON_ISSUED"
			? "Amazon issued"
			: type || "—";
}

export function formatEpoch(seconds: number | undefined) {
	return seconds ? new Date(seconds * 1000).toISOString() : "—";
}

// utf8Base64 encodes text as the base64 that blob members carry.
export function utf8Base64(value: string) {
	return btoa(
		Array.from(new TextEncoder().encode(value), (b) =>
			String.fromCharCode(b),
		).join(""),
	);
}

export function isValidDomain(name: string) {
	if (!name || name.length > 253) return false;
	const labels = name.replace(/^\*\./, "").split(".");
	return (
		labels.length >= 2 &&
		labels.every((l) => /^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$/.test(l))
	);
}

// addName appends a trimmed name once; it returns the list and an error text.
export function addName(
	names: string[],
	domain: string,
	input: string,
): { names: string[]; error: string } {
	const name = input.trim();
	if (!name) return { names, error: "Enter a domain name to add." };
	if (!isValidDomain(name))
		return { names, error: `${name} is not a fully qualified domain name.` };
	if (name === domain.trim() || names.includes(name))
		return { names, error: `${name} is already in the request.` };
	if (names.length + 1 >= 100)
		return {
			names,
			error:
				"A certificate can have at most 100 names, including the domain name.",
		};
	return { names: [...names, name], error: "" };
}

export function removeName(names: string[], name: string) {
	return names.filter((n) => n !== name);
}

export function parseTags(text: string): { Key: string; Value: string }[] {
	if (!text.trim()) return [];
	let value: unknown;
	try {
		value = JSON.parse(text);
	} catch {
		throw new Error("Tags must be a valid JSON object.");
	}
	if (!value || typeof value !== "object" || Array.isArray(value))
		throw new Error("Tags must be a JSON object of string values.");
	const entries = Object.entries(value as Record<string, unknown>);
	if (entries.some(([, v]) => typeof v !== "string"))
		throw new Error("Tag values must be strings.");
	return entries.map(([Key, Value]) => ({ Key, Value: Value as string }));
}

export type RequestForm = {
	domain: string;
	names: string[];
	method: string;
	algorithm: string;
	exportable: boolean;
	transparency: boolean;
	tags: string;
};

export function requestInput(form: RequestForm): Record<string, unknown> {
	const domain = form.domain.trim();
	if (!isValidDomain(domain))
		throw new Error("Enter a fully qualified domain name such as example.com.");
	const tags = parseTags(form.tags);
	return {
		DomainName: domain,
		...(form.names.length ? { SubjectAlternativeNames: form.names } : {}),
		ValidationMethod: form.method,
		KeyAlgorithm: form.algorithm,
		Options: {
			CertificateTransparencyLoggingPreference: form.transparency
				? "ENABLED"
				: "DISABLED",
			Export: form.exportable ? "ENABLED" : "DISABLED",
		},
		...(tags.length ? { Tags: tags } : {}),
	};
}

const certificatePem =
	/-----BEGIN CERTIFICATE-----[\s\S]+-----END CERTIFICATE-----/;
const privateKeyPem =
	/-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]+-----END [A-Z ]*PRIVATE KEY-----/;

export function importInput(form: {
	arn: string;
	certificate: string;
	privateKey: string;
	chain: string;
	tags: string;
}): Record<string, unknown> {
	const certificate = form.certificate.trim(),
		key = form.privateKey.trim(),
		chain = form.chain.trim();
	if (!certificatePem.test(certificate))
		throw new Error("Paste the PEM certificate body (BEGIN CERTIFICATE).");
	if (!privateKeyPem.test(key))
		throw new Error("Paste the PEM private key (BEGIN PRIVATE KEY).");
	if (chain && !certificatePem.test(chain))
		throw new Error("The certificate chain must be PEM certificates.");
	const tags = parseTags(form.tags);
	if (form.arn && tags.length)
		throw new Error("Tags cannot be applied when reimporting a certificate.");
	return {
		...(form.arn ? { CertificateArn: form.arn } : {}),
		Certificate: utf8Base64(`${certificate}\n`),
		PrivateKey: utf8Base64(`${key}\n`),
		...(chain ? { CertificateChain: utf8Base64(`${chain}\n`) } : {}),
		...(tags.length ? { Tags: tags } : {}),
	};
}

export function passphraseError(passphrase: string) {
	const bytes = new TextEncoder().encode(passphrase).length;
	return bytes < passphraseMin || bytes > passphraseMax
		? `The passphrase must be ${passphraseMin} to ${passphraseMax} bytes.`
		: "";
}

export function pemFileName(domain: string | undefined, suffix: string) {
	const base = (domain ?? "certificate")
		.replace(/^\*\./, "wildcard.")
		.replace(/[^a-zA-Z0-9.-]/g, "_");
	return `${base}${suffix}.pem`;
}

export function maskKey(key: string, reveal: boolean) {
	return reveal ? key : "••••••••••••••••";
}
