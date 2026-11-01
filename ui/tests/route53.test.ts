import assert from "node:assert/strict";
import test from "node:test";
import {
	buildHealthCheckConfig,
	buildRecordSet,
	changeBatch,
	emptyHealthCheckForm,
	emptyRecordForm,
	formatValues,
	isProtectedRecord,
	parseValues,
	quoteText,
	recordFullName,
	recordToForm,
	relativeName,
} from "../src/lib/route53Helpers.ts";

const zone = "example.com.";

test("record names resolve against the zone", () => {
	assert.equal(recordFullName("", zone), zone);
	assert.equal(recordFullName("@", zone), zone);
	assert.equal(recordFullName("WWW", zone), "www.example.com.");
	assert.equal(recordFullName("www.example.com", zone), "www.example.com.");
	assert.equal(recordFullName("a.example.com.", zone), "a.example.com.");
	assert.equal(
		recordFullName("notexample.com", zone),
		"notexample.com.example.com.",
	);
	assert.equal(relativeName("www.example.com.", zone), "www");
	assert.equal(relativeName(zone, zone), "@");
});

test("values parse one per line and round-trip", () => {
	assert.deepEqual(parseValues(" 1.1.1.1 \n\n2.2.2.2\n"), [
		"1.1.1.1",
		"2.2.2.2",
	]);
	assert.equal(formatValues([{ Value: "a" }, { Value: "b" }]), "a\nb");
	assert.equal(formatValues(undefined), "");
});

test("text values are quoted once", () => {
	assert.equal(quoteText("v=spf1 -all"), '"v=spf1 -all"');
	assert.equal(quoteText('"done"'), '"done"');
	assert.equal(quoteText('say "hi"'), '"say \\"hi\\""');
});

test("buildRecordSet builds plain and alias records", () => {
	const plain = buildRecordSet(
		{
			...emptyRecordForm,
			name: "www",
			type: "TXT",
			ttl: "60",
			values: "hello",
		},
		zone,
	);
	assert.deepEqual(plain, {
		Name: "www.example.com.",
		Type: "TXT",
		TTL: 60,
		ResourceRecords: [{ Value: '"hello"' }],
	});
	const alias = buildRecordSet(
		{
			...emptyRecordForm,
			name: "app",
			alias: true,
			aliasDNSName: "target.example.com",
			aliasZoneId: "/hostedzone/Z123",
			evaluateTargetHealth: true,
		},
		zone,
	);
	assert.deepEqual(alias.AliasTarget, {
		DNSName: "target.example.com",
		HostedZoneId: "Z123",
		EvaluateTargetHealth: true,
	});
	assert.equal(alias.TTL, undefined);
});

test("buildRecordSet rejects bad input", () => {
	const bad = (patch: object) =>
		assert.throws(() =>
			buildRecordSet({ ...emptyRecordForm, values: "1.1.1.1", ...patch }, zone),
		);
	bad({ values: "" });
	bad({ ttl: "-1" });
	bad({ ttl: "abc" });
	bad({ type: "CNAME", values: "a.\nb." });
	bad({ type: "NS", alias: true, aliasDNSName: "x", aliasZoneId: "Z1" });
	bad({ alias: true });
	bad({ type: "SOA" });
});

test("editing keeps the identity and routing of the record", () => {
	const existing = {
		Name: "w.example.com.",
		Type: "A",
		SetIdentifier: "one",
		Weight: 5,
		TTL: 30,
		ResourceRecords: [{ Value: "1.1.1.1" }],
	};
	const next = buildRecordSet(
		{
			...recordToForm(existing, zone),
			name: "ignored",
			values: "2.2.2.2",
			ttl: "90",
		},
		zone,
		existing,
	);
	assert.deepEqual(next, {
		...existing,
		TTL: 90,
		ResourceRecords: [{ Value: "2.2.2.2" }],
	});
});

test("change batches carry one change", () => {
	const rs = {
		Name: "a.example.com.",
		Type: "A",
		TTL: 1,
		ResourceRecords: [{ Value: "1.1.1.1" }],
	};
	assert.deepEqual(changeBatch("DELETE", rs), {
		Changes: [{ Action: "DELETE", ResourceRecordSet: rs }],
	});
	assert.equal(changeBatch("UPSERT", rs, "note").Comment, "note");
});

test("the apex SOA and NS are protected", () => {
	assert.equal(
		isProtectedRecord({ Name: zone, Type: "SOA" }, "example.com"),
		true,
	);
	assert.equal(isProtectedRecord({ Name: zone, Type: "NS" }, zone), true);
	assert.equal(isProtectedRecord({ Name: zone, Type: "A" }, zone), false);
	assert.equal(
		isProtectedRecord({ Name: `x.${zone}`, Type: "NS" }, zone),
		false,
	);
});

test("health check configs validate", () => {
	assert.deepEqual(
		buildHealthCheckConfig({
			...emptyHealthCheckForm,
			domain: "a.example.com",
			port: "8080",
		}),
		{
			Type: "HTTP",
			FullyQualifiedDomainName: "a.example.com",
			Port: 8080,
			ResourcePath: "/",
			RequestInterval: 30,
			FailureThreshold: 3,
		},
	);
	const ip = { ...emptyHealthCheckForm, ipAddress: "1.1.1.1" };
	assert.throws(() => buildHealthCheckConfig(emptyHealthCheckForm));
	assert.throws(() => buildHealthCheckConfig({ ...ip, type: "TCP" }));
	assert.throws(() =>
		buildHealthCheckConfig({ ...ip, type: "HTTP_STR_MATCH" }),
	);
	assert.throws(() => buildHealthCheckConfig({ ...ip, interval: "20" }));
});
