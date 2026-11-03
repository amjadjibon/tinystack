import assert from "node:assert/strict";
import test from "node:test";
import { unixTime } from "../src/lib/format.ts";
import {
	capacityText,
	configProcessing,
	connectCommand,
	createDomainInput,
	creationDate,
	type DomainConfig,
	type DomainForm,
	type DomainStatus,
	defaultEngineVersion,
	defaultInstanceType,
	defaultTLSPolicy,
	domainState,
	emptyDomainForm,
	enabledText,
	engineVersions,
	formFromDomain,
	hasPendingDomains,
	instanceTypes,
	inventoryFailure,
	mastersText,
	optionRows,
	optionStateType,
	policyText,
	prettyPolicy,
	runtimeHeader,
	runtimeNote,
	stateLabels,
	stateType,
	updateDomainInput,
	valueText,
	volumeTypeChoices,
	volumeTypes,
} from "../src/lib/opensearchHelpers.ts";

const named = (patch: Partial<DomainForm> = {}): DomainForm => ({
	...emptyDomainForm(),
	name: "search-one",
	...patch,
});

const policy = '{"Version":"2012-10-17","Statement":[]}';

test("a blank domain is what the service creates when nothing is set", () => {
	assert.deepEqual(createDomainInput(named()), {
		DomainName: "search-one",
		EngineVersion: "OpenSearch_2.11",
		ClusterConfig: {
			InstanceType: "m5.large.search",
			InstanceCount: 1,
			ZoneAwarenessEnabled: false,
			DedicatedMasterEnabled: false,
		},
		EBSOptions: { EBSEnabled: true, VolumeType: "gp2", VolumeSize: 10 },
		EncryptionAtRestOptions: { Enabled: false },
		NodeToNodeEncryptionOptions: { Enabled: false },
		DomainEndpointOptions: {
			EnforceHTTPS: false,
			TLSSecurityPolicy: "Policy-Min-TLS-1-0-2019-07",
		},
	});
	assert.equal(defaultEngineVersion, "OpenSearch_2.11");
	assert.equal(defaultInstanceType, "m5.large.search");
	assert.equal(defaultTLSPolicy, "Policy-Min-TLS-1-0-2019-07");
});

test("a domain carries every setting the form sets", () => {
	const input = createDomainInput(
		named({
			name: " search-two ",
			engineVersion: "Elasticsearch_7.10",
			instanceType: "r6g.large.search",
			instanceCount: " 4 ",
			zoneAwareness: true,
			dedicatedMaster: true,
			masterType: "m5.xlarge.search",
			masterCount: "3",
			volumeType: "gp3",
			volumeSize: "100",
			iops: "3000",
			throughput: "125",
			policy: ` { "Version": "2012-10-17",\n "Statement": [] } `,
			atRest: true,
			kmsKey: " alias/search ",
			nodeToNode: true,
			enforceHTTPS: true,
			tlsPolicy: "Policy-Min-TLS-1-2-2019-07",
			tags: [
				{ key: "team", value: "search" },
				{ key: " ", value: "" },
			],
		}),
	);
	assert.deepEqual(input, {
		DomainName: "search-two",
		EngineVersion: "Elasticsearch_7.10",
		ClusterConfig: {
			InstanceType: "r6g.large.search",
			InstanceCount: 4,
			ZoneAwarenessEnabled: true,
			DedicatedMasterEnabled: true,
			DedicatedMasterType: "m5.xlarge.search",
			DedicatedMasterCount: 3,
		},
		EBSOptions: {
			EBSEnabled: true,
			VolumeType: "gp3",
			VolumeSize: 100,
			Iops: 3000,
			Throughput: 125,
		},
		EncryptionAtRestOptions: { Enabled: true, KmsKeyId: "alias/search" },
		NodeToNodeEncryptionOptions: { Enabled: true },
		DomainEndpointOptions: {
			EnforceHTTPS: true,
			TLSSecurityPolicy: "Policy-Min-TLS-1-2-2019-07",
		},
		AccessPolicies: policy,
		TagList: [{ Key: "team", Value: "search" }],
	});
});

test("settings a choice turns off are left out", () => {
	const input = createDomainInput(
		named({
			// The master settings stay in the form after the toggle is turned off.
			dedicatedMaster: false,
			masterType: "m5.xlarge.search",
			masterCount: "5",
			// IOPS and throughput are hidden for gp2 volumes.
			volumeType: "gp2",
			iops: "3000",
			throughput: "125",
			atRest: false,
			kmsKey: "alias/unused",
		}),
	);
	assert.deepEqual(input.ClusterConfig, {
		InstanceType: "m5.large.search",
		InstanceCount: 1,
		ZoneAwarenessEnabled: false,
		DedicatedMasterEnabled: false,
	});
	assert.deepEqual(input.EBSOptions, {
		EBSEnabled: true,
		VolumeType: "gp2",
		VolumeSize: 10,
	});
	assert.deepEqual(input.EncryptionAtRestOptions, { Enabled: false });
	// io1 takes IOPS but not throughput.
	assert.deepEqual(
		createDomainInput(
			named({ volumeType: "io1", iops: "1000", throughput: "125" }),
		).EBSOptions,
		{ EBSEnabled: true, VolumeType: "io1", VolumeSize: 10, Iops: 1000 },
	);
	// Instance storage has no volume settings.
	assert.deepEqual(
		createDomainInput(named({ ebsEnabled: false, volumeSize: "bad" }))
			.EBSOptions,
		{ EBSEnabled: false },
	);
	// An empty policy and no tags are not sent.
	assert.equal("AccessPolicies" in input, false);
	assert.equal("TagList" in input, false);
});

test("creation rejects what the service would refuse", () => {
	const refuse = (patch: Partial<DomainForm>, message: RegExp) =>
		assert.throws(() => createDomainInput(named(patch)), message);
	for (const name of [
		"",
		"ab",
		"Search",
		"1search",
		"search_one",
		"s".repeat(29),
	])
		refuse({ name }, /Domain name/);
	assert.ok(createDomainInput(named({ name: "s".repeat(28) })));
	assert.ok(createDomainInput(named({ name: "s-1" })));
	refuse({ engineVersion: "" }, /engine version/);
	refuse({ engineVersion: "OpenSearch_2" }, /engine version/);
	refuse({ engineVersion: "Solr_9.1" }, /engine version/);
	refuse({ instanceType: "" }, /instance type/);
	refuse({ instanceType: "m5.large" }, /instance type/);
	for (const instanceCount of ["", "0", "-1", "1.5", "x", "1e2", "1001"])
		refuse({ instanceCount }, /Instance count/);
	refuse({ instanceCount: "3", zoneAwareness: true }, /even/);
	assert.ok(
		createDomainInput(named({ instanceCount: "2", zoneAwareness: true })),
	);
	refuse({ dedicatedMaster: true, masterType: "m5" }, /master instance type/);
	for (const masterCount of ["", "0", "10", "two"])
		refuse({ dedicatedMaster: true, masterCount }, /Dedicated master count/);
	refuse({ volumeType: "" }, /volume type/);
	refuse({ volumeType: "io2" }, /volume type/);
	for (const volumeSize of ["", "9", "16385", "1.5", "ten"])
		refuse({ volumeSize }, /Volume size/);
	refuse({ volumeType: "gp3", iops: "fast" }, /IOPS/);
	refuse({ volumeType: "gp3", throughput: "0" }, /Throughput/);
	refuse({ atRest: true, kmsKey: "k".repeat(501) }, /KMS key/);
	refuse({ tlsPolicy: "Policy-Min-TLS-1-3" }, /TLS security policy/);
	refuse({ policy: "not json" }, /Access policy must be valid JSON/);
	refuse({ policy: "[]" }, /Access policy must be a JSON object/);
	refuse({ tags: [{ key: "", value: "v" }] }, /needs a key/);
	refuse(
		{
			tags: [
				{ key: "a", value: "1" },
				{ key: "a", value: "2" },
			],
		},
		/repeated/,
	);
});

test("the choices cover the versions, types and policies the service takes", () => {
	assert.ok(engineVersions.includes(defaultEngineVersion));
	assert.ok(instanceTypes.includes(defaultInstanceType));
	for (const version of engineVersions)
		assert.doesNotThrow(() =>
			createDomainInput(named({ engineVersion: version })),
		);
	for (const type of instanceTypes)
		assert.doesNotThrow(() => createDomainInput(named({ instanceType: type })));
	for (const type of volumeTypes)
		assert.doesNotThrow(() => createDomainInput(named({ volumeType: type })));
});

test("access policies are checked and written on one line", () => {
	assert.equal(policyText("  "), "");
	assert.equal(policyText(`{ "a": [1, 2] }`), '{"a":[1,2]}');
	assert.throws(() => policyText("{"), /valid JSON/);
	assert.throws(() => policyText('"allow"'), /JSON object/);
	assert.equal(prettyPolicy(undefined), "");
	assert.equal(prettyPolicy('{"a":1}'), '{\n  "a": 1\n}');
	assert.equal(prettyPolicy("not json"), "not json");
});

const domain: DomainStatus = {
	DomainName: "search-one",
	ARN: "arn:aws:es:eu-west-1:000000000000:domain/search-one",
	DomainId: "000000000000/search-one",
	EngineVersion: "OpenSearch_2.19",
	Endpoint: "search-one-abc.eu-west-1.es.amazonaws.com",
	Created: true,
	Deleted: false,
	Processing: false,
	AccessPolicies: policy,
	ClusterConfig: {
		InstanceType: "m5.large.search",
		InstanceCount: 2,
		ZoneAwarenessEnabled: true,
		DedicatedMasterEnabled: true,
		DedicatedMasterType: "m5.xlarge.search",
		DedicatedMasterCount: 3,
	},
	EBSOptions: {
		EBSEnabled: true,
		VolumeType: "gp3",
		VolumeSize: 20,
		Iops: 3000,
		Throughput: 125,
	},
	EncryptionAtRestOptions: { Enabled: true, KmsKeyId: "alias/search" },
	NodeToNodeEncryptionOptions: { Enabled: true },
	DomainEndpointOptions: {
		EnforceHTTPS: true,
		TLSSecurityPolicy: "Policy-Min-TLS-1-2-2019-07",
	},
};

test("the edit form starts from the settings the domain has", () => {
	const form = formFromDomain(domain);
	assert.deepEqual(form, {
		name: "search-one",
		engineVersion: "OpenSearch_2.19",
		instanceType: "m5.large.search",
		instanceCount: "2",
		dedicatedMaster: true,
		masterType: "m5.xlarge.search",
		masterCount: "3",
		zoneAwareness: true,
		ebsEnabled: true,
		volumeType: "gp3",
		volumeSize: "20",
		iops: "3000",
		throughput: "125",
		policy: JSON.stringify(JSON.parse(policy), null, 2),
		atRest: true,
		kmsKey: "alias/search",
		nodeToNode: true,
		enforceHTTPS: true,
		tlsPolicy: "Policy-Min-TLS-1-2-2019-07",
		tags: [],
	});
	// A domain without optional members starts from the service's defaults.
	const bare = formFromDomain({ DomainName: "bare" });
	assert.equal(bare.instanceCount, "1");
	assert.equal(bare.ebsEnabled, false);
	assert.equal(bare.iops, "");
	assert.equal(bare.tlsPolicy, defaultTLSPolicy);
});

test("an update sends only the groups of settings that changed", () => {
	const form = formFromDomain(domain);
	assert.throws(() => updateDomainInput(domain, form), /Change at least one/);
	assert.deepEqual(updateDomainInput(domain, { ...form, instanceCount: "4" }), {
		DomainName: "search-one",
		ClusterConfig: {
			InstanceType: "m5.large.search",
			InstanceCount: 4,
			ZoneAwarenessEnabled: true,
			DedicatedMasterEnabled: true,
			DedicatedMasterType: "m5.xlarge.search",
			DedicatedMasterCount: 3,
		},
	});
	assert.deepEqual(updateDomainInput(domain, { ...form, volumeSize: "40" }), {
		DomainName: "search-one",
		EBSOptions: {
			EBSEnabled: true,
			VolumeType: "gp3",
			VolumeSize: 40,
			Iops: 3000,
			Throughput: 125,
		},
	});
	assert.deepEqual(
		updateDomainInput(domain, {
			...form,
			enforceHTTPS: false,
			tlsPolicy: "Policy-Min-TLS-1-2-PFS-2023-10",
		}),
		{
			DomainName: "search-one",
			DomainEndpointOptions: {
				EnforceHTTPS: false,
				TLSSecurityPolicy: "Policy-Min-TLS-1-2-PFS-2023-10",
			},
		},
	);
	assert.deepEqual(
		updateDomainInput(domain, { ...form, kmsKey: "alias/other" }),
		{
			DomainName: "search-one",
			EncryptionAtRestOptions: { Enabled: true, KmsKeyId: "alias/other" },
		},
	);
});

test("an update turns dedicated masters and storage off by saying so", () => {
	const form = formFromDomain(domain);
	// The service drops the type and count when it is told the masters are off, and refuses them otherwise.
	assert.deepEqual(
		updateDomainInput(domain, { ...form, dedicatedMaster: false })
			.ClusterConfig,
		{
			InstanceType: "m5.large.search",
			InstanceCount: 2,
			ZoneAwarenessEnabled: true,
			DedicatedMasterEnabled: false,
		},
	);
	assert.deepEqual(
		updateDomainInput(domain, { ...form, ebsEnabled: false }).EBSOptions,
		{ EBSEnabled: false },
	);
});

test("an update replaces or removes the access policy as a whole", () => {
	const form = formFromDomain(domain);
	assert.deepEqual(
		updateDomainInput(domain, { ...form, policy: '{ "Statement": [] }' }),
		{ DomainName: "search-one", AccessPolicies: '{"Statement":[]}' },
	);
	// The empty text removes the policy.
	assert.deepEqual(updateDomainInput(domain, { ...form, policy: "" }), {
		DomainName: "search-one",
		AccessPolicies: "",
	});
	// A domain without a policy gets one.
	assert.deepEqual(
		updateDomainInput(
			{ ...domain, AccessPolicies: undefined },
			{ ...formFromDomain({ ...domain, AccessPolicies: undefined }), policy },
		),
		{ DomainName: "search-one", AccessPolicies: policy },
	);
	assert.throws(
		() => updateDomainInput(domain, { ...form, policy: "{" }),
		/valid JSON/,
	);
});

test("an update checks the groups it sends and no others", () => {
	const form = formFromDomain(domain);
	assert.throws(
		() => updateDomainInput(domain, { ...form, instanceCount: "x" }),
		/Instance count/,
	);
	assert.throws(
		() => updateDomainInput(domain, { ...form, instanceCount: "3" }),
		/even/,
	);
	assert.throws(
		() => updateDomainInput(domain, { ...form, volumeSize: "5" }),
		/Volume size/,
	);
	// A stored value outside the form's bounds does not stop an edit of another group.
	const large = {
		...domain,
		ClusterConfig: { ...domain.ClusterConfig, InstanceCount: 5000 },
	};
	assert.deepEqual(
		updateDomainInput(large, { ...formFromDomain(large), enforceHTTPS: false }),
		{
			DomainName: "search-one",
			DomainEndpointOptions: {
				EnforceHTTPS: false,
				TLSSecurityPolicy: "Policy-Min-TLS-1-2-2019-07",
			},
		},
	);
});

test("a volume with IOPS or throughput keeps to the types that take them", () => {
	assert.deepEqual(volumeTypeChoices(undefined), volumeTypes);
	assert.deepEqual(
		volumeTypeChoices({ VolumeType: "gp2", VolumeSize: 10 }),
		volumeTypes,
	);
	assert.deepEqual(volumeTypeChoices({ Iops: 1000 }), ["gp3", "io1"]);
	assert.deepEqual(volumeTypeChoices({ Iops: 3000, Throughput: 125 }), ["gp3"]);
	assert.deepEqual(volumeTypeChoices({ Throughput: 125 }), ["gp3"]);
});

test("a domain's state follows its flags", () => {
	const active = { Endpoint: "e", Processing: false, Deleted: false };
	assert.equal(domainState(active), "active");
	assert.equal(domainState({ ...active, Processing: true }), "processing");
	assert.equal(
		domainState({ Endpoint: undefined, Processing: true, Deleted: false }),
		"processing",
	);
	assert.equal(
		domainState({ ...active, Deleted: true, Processing: true }),
		"deleting",
	);
	assert.equal(domainState({ ...active, Deleted: true }), "deleting");
	// Without an endpoint a domain that is not processing never got its engine to answer.
	assert.equal(domainState({ Processing: false, Deleted: false }), "failed");
	assert.equal(domainState({}), "failed");

	assert.equal(stateType("active"), "success");
	assert.equal(stateType("processing"), "in-progress");
	assert.equal(stateType("deleting"), "in-progress");
	assert.equal(stateType("failed"), "error");
	assert.equal(stateLabels.processing, "Processing");
});

test("polling runs while a domain starts or is deleted and stops when settled", () => {
	const settled = { Endpoint: "e", Processing: false, Deleted: false };
	assert.equal(hasPendingDomains([]), false);
	assert.equal(hasPendingDomains([settled]), false);
	assert.equal(
		hasPendingDomains([{ Processing: false, Deleted: false }]),
		false,
	);
	assert.equal(
		hasPendingDomains([settled, { ...settled, Processing: true }]),
		true,
	);
	assert.equal(
		hasPendingDomains([settled, { ...settled, Deleted: true }]),
		true,
	);
});

const config: DomainConfig = {
	EngineVersion: {
		Status: {
			State: "Active",
			CreationDate: 1700000100,
			UpdateDate: 1700000200,
		},
	},
	ClusterConfig: {
		Status: {
			State: "Active",
			CreationDate: 1700000000,
			UpdateDate: 1700000300,
			UpdateVersion: 3,
		},
	},
	AccessPolicies: { Status: { State: "Active", CreationDate: 1700000050 } },
};

test("configuration states show pending changes and polling follows them", () => {
	assert.equal(configProcessing(undefined), false);
	assert.equal(configProcessing({}), false);
	assert.equal(configProcessing(config), false);
	assert.equal(
		configProcessing({
			...config,
			EBSOptions: { Status: { State: "Processing" } },
		}),
		true,
	);
	assert.equal(
		configProcessing({
			...config,
			EBSOptions: { Status: { PendingDeletion: true } },
		}),
		true,
	);
	// An option that needs a person to act is not still moving.
	assert.equal(
		configProcessing({
			...config,
			EBSOptions: { Status: { State: "RequiresIndexDocuments" } },
		}),
		false,
	);

	assert.equal(optionStateType({ State: "Active" }), "success");
	assert.equal(optionStateType({ State: "Processing" }), "in-progress");
	assert.equal(
		optionStateType({ State: "Active", PendingDeletion: true }),
		"in-progress",
	);
	assert.equal(optionStateType({ State: "RequiresIndexDocuments" }), "warning");
	assert.equal(optionStateType({ State: "Other" }), "stopped");
	assert.equal(optionStateType(undefined), "stopped");
});

test("every option has a row, in the order of the page", () => {
	const rows = optionRows(config);
	assert.deepEqual(
		rows.map((r) => r.label),
		[
			"Engine version",
			"Cluster configuration",
			"EBS storage",
			"Access policy",
			"Encryption at rest",
			"Node-to-node encryption",
			"Domain endpoint",
		],
	);
	assert.equal(rows[1].status?.UpdateVersion, 3);
	assert.equal(rows[2].status, undefined);
	assert.equal(optionRows(undefined).length, 7);
});

test("a domain was created when its first option was", () => {
	assert.equal(creationDate(config), 1700000000);
	assert.equal(creationDate({}), undefined);
	assert.equal(creationDate(undefined), undefined);
	assert.equal(
		creationDate({ EBSOptions: { Status: { CreationDate: 0 } } }),
		undefined,
	);
	// Timestamps use the viewer's locale and a dash when absent.
	assert.equal(unixTime(creationDate({})), "-");
	assert.equal(
		unixTime(creationDate(config)),
		new Date(1700000000000).toLocaleString(),
	);
});

test("settings read as short text", () => {
	assert.equal(capacityText(domain.ClusterConfig), "2 × m5.large.search");
	assert.equal(capacityText(undefined), "-");
	assert.equal(capacityText({ InstanceCount: 1 }), "-");
	assert.equal(mastersText(domain.ClusterConfig), "3 × m5.xlarge.search");
	assert.equal(mastersText({ DedicatedMasterEnabled: false }), "Not used");
	assert.equal(mastersText(undefined), "Not used");
	assert.equal(valueText(undefined), "-");
	assert.equal(valueText(""), "-");
	assert.equal(valueText(0), "0");
	assert.equal(valueText(20, " GiB"), "20 GiB");
	assert.equal(valueText("gp3"), "gp3");
	assert.equal(enabledText(true), "Enabled");
	assert.equal(enabledText(false), "Disabled");
	assert.equal(enabledText(undefined), "Disabled");
});

test("the page says which runtime serves the endpoints", () => {
	assert.equal(runtimeHeader("docker"), "Docker runtime configured");
	assert.equal(runtimeHeader("records"), "Records-only runtime");
	assert.equal(runtimeHeader(undefined), "Records-only runtime");
	assert.match(runtimeNote("docker"), /Docker/);
	assert.match(runtimeNote("docker"), /processing until its engine answers/);
	assert.match(runtimeNote("records"), /placeholder/);
	assert.match(runtimeNote(undefined), /opensearch\.domains/);

	// Only a Docker engine can be reached, over plain HTTP on the loopback port the domain reports.
	assert.equal(
		connectCommand("docker", "127.0.0.1:55001"),
		"curl http://127.0.0.1:55001/_cluster/health?pretty",
	);
	assert.equal(connectCommand("docker", undefined), "");
	assert.equal(connectCommand("docker", ""), "");
	assert.equal(
		connectCommand("records", "search-one.eu-west-1.es.amazonaws.com"),
		"",
	);
	assert.equal(connectCommand(undefined, "127.0.0.1:55001"), "");
});

test("a failed domain's reason comes from the inventory", () => {
	const arn = "arn:aws:es:eu-west-1:000000000000:domain/search-one";
	const resources = [
		{ arn: "arn:other", details: { Failure: "not this one" } },
		{ arn, details: { Failure: "container did not start: no docker" } },
	];
	assert.equal(
		inventoryFailure(resources, arn),
		"container did not start: no docker",
	);
	assert.equal(inventoryFailure(resources, "arn:missing"), "");
	assert.equal(inventoryFailure(resources, undefined), "");
	assert.equal(inventoryFailure([{ arn, details: { Failure: 7 } }], arn), "");
	assert.equal(inventoryFailure([{ arn }], arn), "");
	assert.equal(inventoryFailure([], arn), "");
});
