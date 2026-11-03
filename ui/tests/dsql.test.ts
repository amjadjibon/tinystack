import assert from "node:assert/strict";
import test from "node:test";
import {
	type Cluster,
	createInput,
	dsqlHref,
	editable,
	needsPeer,
	peerArns,
	pending,
	proxyPort,
	psqlCommand,
	statusType,
	tokenCommand,
	updateInput,
	witnessRegions,
} from "../src/lib/dsqlHelpers.ts";

const id = "abcdefghijklmnopqrstuvwxyz";
const arn = `arn:aws:dsql:us-east-1:000000000000:cluster/${id}`;
const peer =
	"arn:aws:dsql:us-east-2:000000000000:cluster/zyxwvutsrqponmlkjihgfedcba";
const cluster: Cluster = {
	Identifier: id,
	Arn: arn,
	Status: "ACTIVE",
	DeletionProtectionEnabled: true,
};
const form = {
	protection: true,
	tags: "{}",
	multi: false,
	witness: "",
	peer: "",
};

test("transitional statuses refresh the page until they settle", () => {
	for (const status of [
		"CREATING",
		"UPDATING",
		"DELETING",
		"PENDING_SETUP",
		"PENDING_DELETE",
	])
		assert.equal(pending(status), true, status);
	assert.equal(pending("ACTIVE"), false);
	assert.equal(pending(undefined), false);
});

test("only ACTIVE and PENDING_SETUP clusters accept an update or delete", () => {
	assert.equal(editable("ACTIVE"), true);
	assert.equal(editable("PENDING_SETUP"), true);
	for (const status of ["CREATING", "UPDATING", "DELETING", "PENDING_DELETE"])
		assert.equal(editable(status), false, status);
	assert.equal(editable(undefined), false);
});

test("status indicators separate settled, moving and waiting clusters", () => {
	assert.equal(statusType("ACTIVE"), "success");
	for (const status of ["CREATING", "UPDATING", "DELETING"])
		assert.equal(statusType(status), "in-progress", status);
	for (const status of ["PENDING_SETUP", "PENDING_DELETE"])
		assert.equal(statusType(status), "pending", status);
	assert.equal(statusType("SOMETHING_ELSE"), "info");
});

test("links name the scope, tab and cluster", () => {
	const url = new URL(
		dsqlHref({ account: "000000000000", region: "us-east-1" }, id, "tags"),
		"http://x",
	);
	assert.equal(url.pathname, "/services/dsql");
	assert.deepEqual(Object.fromEntries(url.searchParams), {
		account: "000000000000",
		region: "us-east-1",
		tab: "tags",
		cluster: id,
	});
	assert.equal(
		new URL(
			dsqlHref({ account: "000000000000", region: "us-east-1" }),
			"http://x",
		).searchParams.has("cluster"),
		false,
	);
});

test("witness Regions are the other Regions of the cluster's set", () => {
	const regions = witnessRegions("us-east-1");
	assert.ok(regions.includes("us-west-2"));
	assert.ok(!regions.includes("us-east-1"));
	assert.ok(!regions.includes("eu-west-1"));
	assert.ok(witnessRegions("eu-west-1").includes("eu-central-1"));
	assert.ok(witnessRegions("ap-south-1").includes("ap-northeast-1"));
	assert.deepEqual(witnessRegions("sa-east-1"), []);
});

test("a standalone cluster sends its protection and only tags it has", () => {
	assert.deepEqual(createInput(form, "us-east-1"), {
		DeletionProtectionEnabled: true,
	});
	assert.deepEqual(
		createInput(
			{ ...form, protection: false, tags: '{"env":"test"}' },
			"us-east-1",
		),
		{ DeletionProtectionEnabled: false, Tags: { env: "test" } },
	);
	assert.equal(
		Object.hasOwn(createInput(form, "us-east-1"), "Tags"),
		false,
		"an empty tag map is left out",
	);
	assert.throws(() => createInput({ ...form, tags: "{" }, "us-east-1"));
	assert.throws(() => createInput({ ...form, tags: '{"a":1}' }, "us-east-1"));
	assert.throws(() => createInput({ ...form, tags: "[]" }, "us-east-1"));
});

test("a multi-Region cluster needs a witness Region from its set", () => {
	const multi = { ...form, multi: true, witness: "us-west-2" };
	assert.deepEqual(createInput(multi, "us-east-1").MultiRegionProperties, {
		WitnessRegion: "us-west-2",
	});
	assert.deepEqual(
		createInput({ ...multi, peer: ` ${peer} ` }, "us-east-1")
			.MultiRegionProperties,
		{ WitnessRegion: "us-west-2", Clusters: [peer] },
	);
	for (const witness of ["", "us-east-1", "eu-west-1", "nowhere"])
		assert.throws(
			() => createInput({ ...multi, witness }, "us-east-1"),
			/witness Region/,
			witness,
		);
	assert.throws(
		() => createInput({ ...multi, witness: "us-west-2" }, "sa-east-1"),
		/witness Region/,
	);
	for (const bad of ["not-an-arn", `${arn}/extra`, "arn:aws:s3:::bucket"])
		assert.throws(
			() => createInput({ ...multi, peer: bad }, "us-east-1"),
			/peer cluster's ARN/,
			bad,
		);
});

test("witness and peer are ignored unless the cluster is multi-Region", () => {
	assert.equal(
		Object.hasOwn(
			createInput(
				{ ...form, witness: "us-west-2", peer: "not-an-arn" },
				"us-east-1",
			),
			"MultiRegionProperties",
		),
		false,
	);
});

test("an update sends only what changed", () => {
	assert.deepEqual(updateInput(cluster, { protection: false, peer: "" }), {
		Identifier: id,
		DeletionProtectionEnabled: false,
	});
	assert.deepEqual(
		updateInput(
			{ ...cluster, DeletionProtectionEnabled: false },
			{ protection: true, peer: "" },
		),
		{ Identifier: id, DeletionProtectionEnabled: true },
	);
	assert.throws(
		() => updateInput(cluster, { protection: true, peer: "" }),
		/at least one setting/,
	);
});

test("a multi-Region cluster is paired by entering its peer once", () => {
	const waiting: Cluster = {
		...cluster,
		Status: "PENDING_SETUP",
		MultiRegionProperties: { WitnessRegion: "us-west-2", Clusters: [arn] },
	};
	assert.equal(needsPeer(waiting), true);
	assert.deepEqual(peerArns(waiting), []);
	assert.deepEqual(updateInput(waiting, { protection: true, peer }), {
		Identifier: id,
		MultiRegionProperties: { Clusters: [peer] },
	});
	assert.throws(
		() => updateInput(waiting, { protection: true, peer: "bad" }),
		/peer cluster's ARN/,
	);
	assert.throws(
		() => updateInput(waiting, { protection: true, peer: "" }),
		/at least one setting/,
	);

	const paired: Cluster = {
		...waiting,
		MultiRegionProperties: {
			WitnessRegion: "us-west-2",
			Clusters: [arn, peer],
		},
	};
	assert.equal(needsPeer(paired), false);
	assert.deepEqual(peerArns(paired), [peer]);
	assert.throws(
		() => updateInput(paired, { protection: true, peer: "arn:other" }),
		/at least one setting/,
		"a paired cluster keeps its peer",
	);
	assert.equal(needsPeer(cluster), false, "a standalone cluster has no peer");
});

test("the proxy port comes from the inventory's loopback address", () => {
	const resource = (address: unknown) => ({
		arn,
		details: { Address: address },
	});
	assert.equal(proxyPort([resource("127.0.0.1:54321")], arn), 54321);
	assert.equal(proxyPort([resource("127.0.0.1:54321")], peer), undefined);
	for (const address of [
		"127.0.0.1:0",
		"127.0.0.1:65536",
		"10.0.0.1:5432",
		"127.0.0.1:",
		"localhost:5432",
		5432,
		undefined,
	])
		assert.equal(proxyPort([resource(address)], arn), undefined, `${address}`);
	assert.equal(proxyPort([{ arn }], arn), undefined);
	assert.equal(proxyPort([], arn), undefined);
});

test("the psql command verifies the endpoint and connects through the proxy", () => {
	const endpoint = `${id}.dsql.us-east-1.on.aws`;
	assert.equal(
		psqlCommand(endpoint, 54321),
		`psql "host=${endpoint} hostaddr=127.0.0.1 port=54321 dbname=postgres user=admin sslmode=verify-full sslrootcert=tinystack-ca.pem"`,
	);
	for (const bad of [
		"",
		'x" ; rm -rf ~ ; "',
		"a b.example",
		"$(whoami).example",
		"-host",
		"HOST.example",
	])
		assert.equal(psqlCommand(bad, 54321), "", bad);
	for (const port of [0, 65536, 1.5, Number.NaN])
		assert.equal(psqlCommand(endpoint, port), "", `${port}`);
});

test("the token command names the cluster endpoint and Region", () => {
	const endpoint = `${id}.dsql.us-east-1.on.aws`;
	assert.equal(
		tokenCommand(endpoint, "us-east-1"),
		`export PGPASSWORD="$(aws dsql generate-db-connect-admin-auth-token --hostname ${endpoint} --region us-east-1)"`,
	);
	assert.equal(tokenCommand("", "us-east-1"), "");
	assert.equal(tokenCommand(endpoint, "us-east-1; id"), "");
	assert.equal(tokenCommand(`${endpoint}"`, "us-east-1"), "");
});
