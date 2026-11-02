import assert from "node:assert/strict";
import test from "node:test";
import {
	discoveryInput,
	dnsRecords,
	namespaceInput,
	pairMap,
	requiredName,
	tagList,
} from "../src/lib/servicediscoveryHelpers.ts";

test("namespace inputs only include the supported fields", () => {
	assert.deepEqual(namespaceInput(" example ", "desc", "HTTP", "ignored"), {
		Name: "example",
		Description: "desc",
	});
	assert.deepEqual(namespaceInput("private", "", "DNS_PRIVATE", "vpc-1"), {
		Name: "private",
		Description: "",
		Vpc: "vpc-1",
	});
	assert.throws(() => namespaceInput("private", "", "DNS_PRIVATE", ""));
	assert.throws(() => namespaceInput("x", "", "bogus", ""));
	assert.throws(() => requiredName(" "));
	assert.throws(() => requiredName("x".repeat(1025)));
});
test("key value editor rejects blank and duplicate keys and safely retains arbitrary keys", () => {
	assert.throws(() => pairMap([{ key: "", value: "x" }]));
	assert.throws(() =>
		pairMap([
			{ key: "a", value: "1" },
			{ key: "a", value: "2" },
		]),
	);
	assert.equal(
		Object.getOwnPropertyDescriptor(
			pairMap([{ key: "__proto__", value: "safe" }]),
			"__proto__",
		)?.value,
		"safe",
	);
	assert.deepEqual(tagList([{ key: "env", value: "dev" }]), [
		{ Key: "env", Value: "dev" },
	]);
});
test("DNS records accept zero TTL and reject unsupported records and invalid numbers", () => {
	assert.deepEqual(
		dnsRecords([
			{ Type: "A", TTL: "0" },
			{ Type: "SRV", TTL: "60" },
		]),
		[
			{ Type: "A", TTL: 0 },
			{ Type: "SRV", TTL: 60 },
		],
	);
	for (const TTL of ["", "-1", "NaN", "1.5", "Infinity"])
		assert.throws(() => dnsRecords([{ Type: "A", TTL }]));
	assert.throws(() => dnsRecords([]));
	assert.throws(() => dnsRecords([{ Type: "TXT", TTL: "60" }]));
});
test("discovery builds required and optional filters and validates health", () => {
	const input = discoveryInput(
		"ns",
		"svc",
		[{ key: "stage", value: "dev" }],
		[{ key: "zone", value: "a" }],
		"HEALTHY_OR_ELSE_ALL",
	);
	assert.equal(input.QueryParameters.stage, "dev");
	assert.equal(input.OptionalParameters.zone, "a");
	assert.equal(input.HealthStatus, "HEALTHY_OR_ELSE_ALL");
	assert.equal(input.MaxResults, 1000);
	assert.throws(() => discoveryInput("ns", "svc", [], [], "invalid"));
	assert.throws(() => discoveryInput("", "svc", [], [], "ALL"));
});
