import assert from "node:assert/strict";
import test from "node:test";
import {
	clusterConfigInputs,
	clusterInput,
	kubeconfig,
	logging,
	pending,
	scaling,
} from "../src/lib/eksHelpers.ts";

test("scaling enforces desired bounds and integer inputs", () => {
	assert.deepEqual(scaling("0", "2", "1"), {
		MinSize: 0,
		MaxSize: 2,
		DesiredSize: 1,
	});
	for (const values of [
		["2", "1", "1"],
		["0", "2", "3"],
		["0.5", "2", "1"],
	])
		assert.throws(() => scaling(values[0], values[1], values[2]));
});
test("cluster creation includes supported endpoint, logging, auth and tags fields", () => {
	const f = {
		name: "demo",
		version: "1.34",
		role: "arn:aws:iam::000000000000:role/eks",
		subnets: ["subnet-a", "subnet-b"],
		groups: [],
		public: true,
		private: false,
		logs: ["api"],
		tags: '{"env":"test"}',
		auth: "API",
	};
	const out = clusterInput(f);
	assert.deepEqual(out.Tags, { env: "test" });
	assert.equal(out.AccessConfig.AuthenticationMode, "API");
	assert.throws(() => clusterInput({ ...f, subnets: [] }));
	assert.throws(() => clusterInput({ ...f, public: false }));
	assert.throws(() => clusterInput({ ...f, role: "bad" }));
	assert.throws(() => clusterInput({ ...f, tags: '{"a":1}' }));
	assert.equal(logging([]).ClusterLogging[0].Enabled, false);
});
test("kubeconfig quotes server values and uses the selected region", () => {
	const config = JSON.parse(
		kubeconfig(
			{
				Name: 'demo"\n',
				Endpoint: "https://localhost:123",
				CertificateAuthority: { Data: "Y2E=" },
			},
			"eu-west-1",
		),
	);
	assert.equal(config.clusters[0].cluster.server, "https://localhost:123");
	assert.equal(
		config.clusters[0].cluster["certificate-authority-data"],
		"Y2E=",
	);
	assert.equal(config.users[0].user.exec.args.at(-1), "eu-west-1");
	assert.equal(config.contexts[0].name, 'demo"\n');
});
test("only transitional states need polling", () => {
	for (const state of ["CREATING", "DELETING", "UPDATING", "InProgress"])
		assert.equal(pending(state), true);
	for (const state of ["ACTIVE", "FAILED", undefined])
		assert.equal(pending(state), false);
});

test("configuration splits categories and refuses authentication downgrades", () => {
	const c = {
		Name: "demo",
		ResourcesVpcConfig: {
			EndpointPublicAccess: true,
			EndpointPrivateAccess: false,
		},
		AccessConfig: { AuthenticationMode: "CONFIG_MAP" },
	};
	const steps = clusterConfigInputs(c, {
		public: true,
		private: true,
		logs: ["api"],
		auth: "API",
	});
	assert.equal(steps.length, 3);
	for (const step of steps) assert.equal(Object.keys(step).length, 2);
	assert.throws(() =>
		clusterConfigInputs(
			{ ...c, AccessConfig: { AuthenticationMode: "API" } },
			{ public: true, private: false, logs: [], auth: "CONFIG_MAP" },
		),
	);
});
