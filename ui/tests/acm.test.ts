import assert from "node:assert/strict";
import test from "node:test";
import {
	addName,
	allKeyTypes,
	certificateType,
	importInput,
	isValidDomain,
	maskKey,
	parseTags,
	passphraseError,
	pemFileName,
	removeName,
	requestInput,
	utf8Base64,
} from "../src/lib/acmHelpers.ts";

const certificate =
	"-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----";
const key = "-----BEGIN PRIVATE KEY-----\nMIIE\n-----END PRIVATE KEY-----";
const form = {
	domain: "example.com",
	names: [],
	method: "DNS",
	algorithm: "RSA_2048",
	exportable: false,
	transparency: true,
	tags: "",
};

test("domain names accept wildcards and reject unqualified names", () => {
	assert.equal(isValidDomain("example.com"), true);
	assert.equal(isValidDomain("*.example.com"), true);
	assert.equal(isValidDomain("localhost"), false);
	assert.equal(isValidDomain("bad_name.example.com"), false);
});

test("additional names are deduplicated, validated and removable", () => {
	const first = addName([], "example.com", " www.example.com ");
	assert.deepEqual(first, { names: ["www.example.com"], error: "" });
	assert.match(
		addName(first.names, "example.com", "www.example.com").error,
		/already/,
	);
	assert.match(addName([], "example.com", "example.com").error, /already/);
	assert.match(addName([], "example.com", "nodots").error, /fully qualified/);
	assert.deepEqual(removeName(first.names, "www.example.com"), []);
});

test("certificate requests carry options, names and tags", () => {
	const input = requestInput({
		...form,
		names: ["www.example.com"],
		exportable: true,
		transparency: false,
		tags: '{"team":"web"}',
	});
	assert.deepEqual(input, {
		DomainName: "example.com",
		SubjectAlternativeNames: ["www.example.com"],
		ValidationMethod: "DNS",
		KeyAlgorithm: "RSA_2048",
		Options: {
			CertificateTransparencyLoggingPreference: "DISABLED",
			Export: "ENABLED",
		},
		Tags: [{ Key: "team", Value: "web" }],
	});
	assert.throws(() => requestInput({ ...form, domain: "localhost" }));
	assert.throws(() => requestInput({ ...form, tags: "[]" }));
	assert.throws(() => parseTags('{"a":1}'));
});

test("imports encode PEM blobs and reject reimports with tags", () => {
	const input = importInput({
		arn: "",
		certificate,
		privateKey: key,
		chain: "",
		tags: "",
	});
	assert.equal(input.Certificate, utf8Base64(`${certificate}\n`));
	assert.equal(Object.hasOwn(input, "CertificateChain"), false);
	assert.equal(Object.hasOwn(input, "CertificateArn"), false);
	assert.equal(
		importInput({
			arn: "arn:x",
			certificate,
			privateKey: key,
			chain: certificate,
			tags: "",
		}).CertificateArn,
		"arn:x",
	);
	assert.throws(() =>
		importInput({
			arn: "arn:x",
			certificate,
			privateKey: key,
			chain: "",
			tags: '{"a":"b"}',
		}),
	);
	assert.throws(() =>
		importInput({
			arn: "",
			certificate: "nope",
			privateKey: key,
			chain: "",
			tags: "",
		}),
	);
	assert.throws(() =>
		importInput({
			arn: "",
			certificate,
			privateKey: "nope",
			chain: "",
			tags: "",
		}),
	);
});

test("passphrases are limited by bytes", () => {
	assert.notEqual(passphraseError("abc"), "");
	assert.equal(passphraseError("abcd"), "");
	assert.notEqual(passphraseError("é".repeat(65)), "");
});

test("labels, file names and key masking", () => {
	assert.equal(certificateType("AMAZON_ISSUED"), "Amazon issued");
	assert.equal(certificateType("IMPORTED"), "Imported");
	assert.equal(
		pemFileName("*.example.com", "-key"),
		"wildcard.example.com-key.pem",
	);
	assert.equal(maskKey("SECRET", true), "SECRET");
	assert.equal(maskKey("SECRET", false).includes("SECRET"), false);
	assert.equal(allKeyTypes.includes("EC_prime256v1"), true);
});
