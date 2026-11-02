import assert from "node:assert/strict";
import test from "node:test";
import {
	applicationArn,
	bytesText,
	type ConfigurationProfile,
	canStop,
	checkDescription,
	checkLabel,
	checkName,
	contentTypeChoices,
	createApplicationInput,
	createEnvironmentInput,
	createProfileInput,
	createStrategyInput,
	createVersionInput,
	type DeploymentStrategy,
	decodeContent,
	deploymentInProgress,
	emptyApplicationForm,
	emptyDeploymentForm,
	emptyProfileForm,
	emptyStrategyForm,
	emptyVersionForm,
	formFromProfile,
	formFromStrategy,
	formFromVersion,
	growthText,
	hasActiveDeployments,
	inventoryTarget,
	isPredefined,
	minutesText,
	newestDeploymentFirst,
	newestVersionFirst,
	percentText,
	prettyContent,
	rolloutText,
	sortByName,
	sortStrategies,
	startDeploymentInput,
	stateStatusType,
	stopDeploymentInput,
	strategyLabel,
	toBase64,
	updateApplicationInput,
	updateEnvironmentInput,
	updateProfileInput,
	updateStrategyInput,
	validatorInputs,
	versionText,
} from "../src/lib/appconfigHelpers.ts";
import { isoTime } from "../src/lib/format.ts";

test("names are trimmed, required and at most 64 characters", () => {
	assert.equal(checkName("  checkout "), "checkout");
	assert.throws(() => checkName("   "), /Name is required/);
	assert.throws(
		() => checkName("", "Strategy name"),
		/Strategy name is required/,
	);
	assert.equal(checkName("a".repeat(64)).length, 64);
	assert.throws(() => checkName("a".repeat(65)), /at most 64 characters/);
	// Characters are counted, not UTF-16 units.
	assert.equal(checkName("😀".repeat(64)), "😀".repeat(64));
	assert.throws(() => checkName("😀".repeat(65)), /at most 64 characters/);
});

test("a description may be empty and has at most 1024 characters", () => {
	assert.equal(checkDescription("  "), "");
	assert.equal(checkDescription(" text "), "text");
	assert.equal(checkDescription("d".repeat(1024)).length, 1024);
	assert.throws(() => checkDescription("d".repeat(1025)), /at most 1024/);
});

test("an application input leaves out what is empty", () => {
	const form = { ...emptyApplicationForm(), name: " checkout " };
	assert.deepEqual(createApplicationInput(form), { Name: "checkout" });
	assert.deepEqual(
		createApplicationInput({
			name: "checkout",
			description: " Checkout service ",
			tags: [
				{ key: "team", value: "core" },
				{ key: " ", value: "" },
			],
		}),
		{
			Name: "checkout",
			Description: "Checkout service",
			Tags: { team: "core" },
		},
	);
	assert.throws(
		() => createApplicationInput(emptyApplicationForm()),
		/required/,
	);
	assert.throws(
		() =>
			createApplicationInput({
				...form,
				tags: [
					{ key: "a", value: "1" },
					{ key: "a", value: "2" },
				],
			}),
		/repeated/,
	);
	assert.throws(
		() => createApplicationInput({ ...form, tags: [{ key: "", value: "v" }] }),
		/needs a key/,
	);
});

test("an edit sends only what changed, and an emptied description clears it", () => {
	const app = { Id: "abc1234", Name: "checkout", Description: "Old" };
	assert.deepEqual(
		updateApplicationInput(app, { name: "checkout", description: "New" }),
		{ ApplicationId: "abc1234", Description: "New" },
	);
	assert.deepEqual(
		updateApplicationInput(app, { name: " billing ", description: "Old" }),
		{ ApplicationId: "abc1234", Name: "billing" },
	);
	assert.deepEqual(
		updateApplicationInput(app, { name: "checkout", description: "" }),
		{ ApplicationId: "abc1234", Description: "" },
	);
	assert.throws(
		() => updateApplicationInput(app, { name: "checkout", description: "Old" }),
		/Change at least one/,
	);
	assert.throws(
		() => updateApplicationInput(app, { name: "", description: "Old" }),
		/required/,
	);
	// A resource without a description is unchanged by an empty one.
	assert.throws(
		() =>
			updateApplicationInput(
				{ Id: "abc1234", Name: "x" },
				{ name: "x", description: "" },
			),
		/Change at least one/,
	);
});

test("environments are created and edited inside their application", () => {
	assert.deepEqual(
		createEnvironmentInput("app1", { name: " prod ", description: "" }),
		{ ApplicationId: "app1", Name: "prod" },
	);
	assert.deepEqual(
		createEnvironmentInput("app1", { name: "prod", description: "Live" }),
		{ ApplicationId: "app1", Name: "prod", Description: "Live" },
	);
	assert.deepEqual(
		updateEnvironmentInput(
			"app1",
			{ Id: "env1", Name: "prod", Description: "Live" },
			{ name: "production", description: "Live" },
		),
		{ ApplicationId: "app1", EnvironmentId: "env1", Name: "production" },
	);
	assert.throws(
		() =>
			updateEnvironmentInput(
				"app1",
				{ Id: "env1", Name: "prod" },
				{ name: "prod", description: "" },
			),
		/Change at least one/,
	);
});

test("an application ARN is built from its account, Region and ID", () => {
	assert.equal(
		applicationArn({ account: "000000000000", region: "eu-west-1" }, "abc1234"),
		"arn:aws:appconfig:eu-west-1:000000000000:application/abc1234",
	);
});

test("a profile is created in the hosted store with its type", () => {
	const form = { ...emptyProfileForm(), name: "flags" };
	assert.deepEqual(createProfileInput("app1", form), {
		ApplicationId: "app1",
		Name: "flags",
		LocationUri: "hosted",
		Type: "AWS.Freeform",
	});
	assert.deepEqual(
		createProfileInput("app1", {
			...form,
			description: "Feature flags",
			type: "AWS.AppConfig.FeatureFlags",
			validators: [{ type: "JSON_SCHEMA", content: ' {"type":"object"} ' }],
		}),
		{
			ApplicationId: "app1",
			Name: "flags",
			LocationUri: "hosted",
			Type: "AWS.AppConfig.FeatureFlags",
			Description: "Feature flags",
			Validators: [{ Type: "JSON_SCHEMA", Content: '{"type":"object"}' }],
		},
	);
	assert.throws(
		() => createProfileInput("app1", { ...form, type: "AWS.Other" }),
		/Choose a configuration type/,
	);
	assert.throws(
		() => createProfileInput("app1", emptyProfileForm()),
		/required/,
	);
});

test("validators are JSON schemas or Lambda function ARNs, at most two", () => {
	const arn = "arn:aws:lambda:eu-west-1:000000000000:function:check";
	assert.deepEqual(
		validatorInputs([
			{ type: "JSON_SCHEMA", content: "{}" },
			{ type: "LAMBDA", content: arn },
		]),
		[
			{ Type: "JSON_SCHEMA", Content: "{}" },
			{ Type: "LAMBDA", Content: arn },
		],
	);
	assert.deepEqual(
		validatorInputs([{ type: "LAMBDA", content: `${arn}:live` }]),
		[{ Type: "LAMBDA", Content: `${arn}:live` }],
	);
	assert.deepEqual(validatorInputs([]), []);
	assert.throws(
		() => validatorInputs([{ type: "XML", content: "{}" }]),
		/Validator 1: choose JSON_SCHEMA or LAMBDA/,
	);
	assert.throws(
		() => validatorInputs([{ type: "JSON_SCHEMA", content: "{" }]),
		/Validator 1: the schema must be valid JSON/,
	);
	assert.throws(
		() => validatorInputs([{ type: "JSON_SCHEMA", content: "[]" }]),
		/must be a JSON object/,
	);
	assert.throws(
		() => validatorInputs([{ type: "JSON_SCHEMA", content: "null" }]),
		/must be a JSON object/,
	);
	assert.throws(
		() => validatorInputs([{ type: "JSON_SCHEMA", content: "" }]),
		/valid JSON/,
	);
	assert.throws(
		() =>
			validatorInputs([
				{ type: "JSON_SCHEMA", content: "{}" },
				{ type: "LAMBDA", content: "my-function" },
			]),
		/Validator 2: enter the ARN of a Lambda function/,
	);
	assert.throws(
		() =>
			validatorInputs([
				{ type: "JSON_SCHEMA", content: "{}" },
				{ type: "JSON_SCHEMA", content: "{}" },
				{ type: "JSON_SCHEMA", content: "{}" },
			]),
		/at most 2 validators/,
	);
	const big = `{"x":"${"a".repeat(32768)}"}`;
	assert.throws(
		() => validatorInputs([{ type: "JSON_SCHEMA", content: big }]),
		/at most 32768 bytes/,
	);
});

test("a profile edit changes the name, description and validators only", () => {
	const profile: ConfigurationProfile = {
		Id: "prof1",
		Name: "flags",
		Description: "Flags",
		Type: "AWS.Freeform",
		LocationUri: "hosted",
		Validators: [{ Type: "JSON_SCHEMA", Content: "{}" }],
	};
	const form = formFromProfile(profile);
	assert.deepEqual(form, {
		name: "flags",
		description: "Flags",
		type: "AWS.Freeform",
		validators: [{ type: "JSON_SCHEMA", content: "{}" }],
	});
	assert.throws(
		() => updateProfileInput("app1", profile, form),
		/Change at least one/,
	);
	assert.deepEqual(
		updateProfileInput("app1", profile, { ...form, description: "Flags v2" }),
		{
			ApplicationId: "app1",
			ConfigurationProfileId: "prof1",
			Description: "Flags v2",
		},
	);
	// Removing every validator sends an empty list, which clears them.
	assert.deepEqual(
		updateProfileInput("app1", profile, { ...form, validators: [] }),
		{
			ApplicationId: "app1",
			ConfigurationProfileId: "prof1",
			Validators: [],
		},
	);
	assert.deepEqual(
		updateProfileInput("app1", profile, {
			...form,
			validators: [{ type: "JSON_SCHEMA", content: '{"type":"object"}' }],
		}).Validators,
		[{ Type: "JSON_SCHEMA", Content: '{"type":"object"}' }],
	);
	assert.deepEqual(formFromProfile({ Id: "p" }).validators, []);
	assert.equal(formFromProfile({ Id: "p" }).type, "AWS.Freeform");
});

test("JSON is re-indented and anything else is left alone", () => {
	assert.equal(prettyContent('{"a":1}'), '{\n  "a": 1\n}');
	assert.equal(prettyContent("not json"), "not json");
});

test("a new strategy names ReplicateTo and checks its numbers", () => {
	const form = { ...emptyStrategyForm(), name: " gradual " };
	assert.deepEqual(createStrategyInput(form), {
		Name: "gradual",
		DeploymentDurationInMinutes: 10,
		FinalBakeTimeInMinutes: 10,
		GrowthFactor: 20,
		GrowthType: "LINEAR",
		ReplicateTo: "NONE",
	});
	assert.deepEqual(
		createStrategyInput({
			...form,
			description: "Slow",
			duration: "1440",
			bake: "0",
			factor: "25.5",
			growth: "EXPONENTIAL",
		}),
		{
			Name: "gradual",
			Description: "Slow",
			DeploymentDurationInMinutes: 1440,
			FinalBakeTimeInMinutes: 0,
			GrowthFactor: 25.5,
			GrowthType: "EXPONENTIAL",
			ReplicateTo: "NONE",
		},
	);
	assert.throws(() => createStrategyInput(emptyStrategyForm()), /required/);
	assert.throws(
		() => createStrategyInput({ ...form, name: "AppConfig.Mine" }),
		/belong to the AWS strategies/,
	);
	for (const factor of ["0", "0.5", "100.1", "abc", "", "-5", "1e2"])
		assert.throws(
			() => createStrategyInput({ ...form, factor }),
			/Growth factor must be a number from 1 to 100/,
			factor,
		);
	assert.equal(
		createStrategyInput({ ...form, factor: "100" }).GrowthFactor,
		100,
	);
	assert.equal(createStrategyInput({ ...form, factor: "1" }).GrowthFactor, 1);
	assert.throws(
		() => createStrategyInput({ ...form, duration: "1441" }),
		/Deployment time must be a whole number from 0 to 1440/,
	);
	assert.throws(
		() => createStrategyInput({ ...form, duration: "-1" }),
		/Deployment time/,
	);
	assert.throws(
		() => createStrategyInput({ ...form, duration: "1.5" }),
		/Deployment time/,
	);
	assert.throws(
		() => createStrategyInput({ ...form, bake: "1441" }),
		/Bake time must be a whole number from 0 to 1440/,
	);
	assert.throws(
		() => createStrategyInput({ ...form, growth: "STEP" }),
		/Choose LINEAR or EXPONENTIAL growth/,
	);
});

test("a strategy edit sends only the settings that changed", () => {
	const strategy: DeploymentStrategy = {
		Id: "str1234",
		Name: "gradual",
		Description: "Slow",
		DeploymentDurationInMinutes: 30,
		FinalBakeTimeInMinutes: 10,
		GrowthFactor: 25.5,
		GrowthType: "EXPONENTIAL",
		ReplicateTo: "NONE",
	};
	const form = formFromStrategy(strategy);
	assert.deepEqual(form, {
		name: "gradual",
		description: "Slow",
		duration: "30",
		bake: "10",
		factor: "25.5",
		growth: "EXPONENTIAL",
	});
	assert.throws(
		() => updateStrategyInput(strategy, form),
		/Change at least one/,
	);
	assert.deepEqual(updateStrategyInput(strategy, { ...form, factor: "50" }), {
		DeploymentStrategyId: "str1234",
		GrowthFactor: 50,
	});
	assert.deepEqual(
		updateStrategyInput(strategy, {
			...form,
			description: "",
			duration: "5",
			bake: "0",
			growth: "LINEAR",
		}),
		{
			DeploymentStrategyId: "str1234",
			Description: "",
			DeploymentDurationInMinutes: 5,
			FinalBakeTimeInMinutes: 0,
			GrowthType: "LINEAR",
		},
	);
	// The name is not editable, so a changed name alone changes nothing.
	assert.throws(
		() => updateStrategyInput(strategy, { ...form, name: "other" }),
		/Change at least one/,
	);
	assert.throws(
		() => updateStrategyInput(strategy, { ...form, factor: "150" }),
		/Growth factor/,
	);
});

test("the AWS strategies are told apart, listed first and cannot be edited", () => {
	assert.equal(isPredefined({ Id: "AppConfig.AllAtOnce" }), true);
	assert.equal(isPredefined({ Id: "abc1234" }), false);
	const sorted = sortStrategies([
		{ Id: "z123456", Name: "zeta" },
		{ Id: "AppConfig.Linear", Name: "AppConfig.Linear" },
		{ Id: "a123456", Name: "alpha" },
		{ Id: "AppConfig.AllAtOnce", Name: "AppConfig.AllAtOnce" },
	]);
	assert.deepEqual(
		sorted.map((s) => s.Name),
		["AppConfig.AllAtOnce", "AppConfig.Linear", "alpha", "zeta"],
	);
	assert.equal(
		strategyLabel({ Id: "AppConfig.AllAtOnce", Name: "AppConfig.AllAtOnce" }),
		"AppConfig.AllAtOnce",
	);
	assert.equal(
		strategyLabel({ Id: "abc1234", Name: "gradual" }),
		"gradual (abc1234)",
	);
	assert.deepEqual(
		sortByName([{ Name: "b" }, { Name: "A" }, {}]).map((x) => x.Name),
		[undefined, "A", "b"],
	);
});

test("rollout settings read as short text", () => {
	assert.equal(
		growthText({ GrowthType: "LINEAR", GrowthFactor: 20 }),
		"LINEAR 20%",
	);
	assert.equal(growthText({}), "-");
	assert.equal(minutesText(0), "0 min");
	assert.equal(minutesText(undefined), "-");
	assert.equal(
		rolloutText({
			GrowthType: "LINEAR",
			GrowthFactor: 50,
			DeploymentDurationInMinutes: 1,
			FinalBakeTimeInMinutes: 1,
		}),
		"LINEAR 50% over 1 min, bake 1 min",
	);
});

test("content is sent as base64 of its UTF-8 bytes", () => {
	assert.equal(toBase64(new TextEncoder().encode("hi")), "aGk=");
	// More than one chunk of bytes still encodes.
	const big = new Uint8Array(100000).fill(65);
	assert.equal(atob(toBase64(big)).length, 100000);
	assert.deepEqual(
		decodeContent(toBase64(new TextEncoder().encode("héllo ✓"))),
		{
			text: "héllo ✓",
			size: 10,
		},
	);
	// Bytes that are not UTF-8 have no text, only a size.
	assert.deepEqual(decodeContent(toBase64(new Uint8Array([0, 1, 2, 255]))), {
		size: 4,
	});
	assert.deepEqual(decodeContent(undefined), { text: "", size: 0 });
	assert.equal(bytesText(1), "1 byte");
	assert.equal(bytesText(4), "4 bytes");
});

test("a version carries its content, type, label and description", () => {
	const form = {
		...emptyVersionForm(),
		content: '{"flag":true}',
		description: " first ",
		label: " stable ",
	};
	assert.deepEqual(createVersionInput("app1", "prof1", form), {
		ApplicationId: "app1",
		ConfigurationProfileId: "prof1",
		Content: toBase64(new TextEncoder().encode('{"flag":true}')),
		ContentType: "application/json",
		Description: "first",
		VersionLabel: "stable",
	});
	assert.deepEqual(
		createVersionInput("app1", "prof1", {
			...emptyVersionForm(),
			contentType: "text/plain",
			content: "hello ✓",
		}),
		{
			ApplicationId: "app1",
			ConfigurationProfileId: "prof1",
			Content: "aGVsbG8g4pyT",
			ContentType: "text/plain",
		},
	);
	// YAML and text are kept as typed, whatever they contain.
	assert.equal(
		createVersionInput("app1", "prof1", {
			...emptyVersionForm(),
			contentType: "application/x-yaml",
			content: "a: [unclosed",
		}).ContentType,
		"application/x-yaml",
	);
});

test("a version is refused when its content or type cannot be right", () => {
	const base = { ...emptyVersionForm(), content: "{}" };
	assert.throws(
		() => createVersionInput("a", "p", { ...base, content: "  \n" }),
		/Enter the configuration content/,
	);
	assert.throws(
		() => createVersionInput("a", "p", { ...base, content: "{oops" }),
		/Content is not valid JSON/,
	);
	assert.throws(
		() => createVersionInput("a", "p", { ...base, contentType: "json" }),
		/Content type must look like/,
	);
	assert.throws(
		() => createVersionInput("a", "p", { ...base, contentType: "" }),
		/Content type must look like/,
	);
	assert.throws(
		() =>
			createVersionInput("a", "p", {
				...base,
				contentType: "text/plain",
				content: "x".repeat(1024 * 1024 + 1),
			}),
		/at most 1 MiB/,
	);
	assert.equal(
		createVersionInput("a", "p", {
			...base,
			contentType: "text/plain",
			content: "x".repeat(1024 * 1024),
		}).ContentType,
		"text/plain",
	);
	assert.throws(
		() =>
			createVersionInput("a", "p", { ...base, description: "d".repeat(1025) }),
		/Description/,
	);
});

test("a label must not read as a version number", () => {
	assert.equal(checkLabel("  "), "");
	assert.equal(checkLabel(" v2 "), "v2");
	assert.equal(checkLabel("2024-01"), "2024-01");
	assert.throws(() => checkLabel("12"), /not a digit/);
	assert.throws(() => checkLabel("a".repeat(65)), /at most 64/);
	assert.equal(checkLabel("a".repeat(64)).length, 64);
});

test("a copied version starts the editor with its content type and text", () => {
	assert.deepEqual(formFromVersion({ ContentType: "text/plain" }, "hello"), {
		contentType: "text/plain",
		content: "hello",
		description: "",
		label: "",
	});
	assert.deepEqual(contentTypeChoices("text/plain"), [
		"application/json",
		"application/x-yaml",
		"text/plain",
	]);
	assert.deepEqual(
		contentTypeChoices(""),
		contentTypeChoices("application/json"),
	);
	assert.deepEqual(
		contentTypeChoices("application/toml").at(-1),
		"application/toml",
	);
	assert.equal(contentTypeChoices("application/toml").length, 4);
});

test("versions and deployments list the newest first", () => {
	assert.deepEqual(
		newestVersionFirst([
			{ VersionNumber: 1 },
			{ VersionNumber: 3 },
			{ VersionNumber: 2 },
		]).map((v) => v.VersionNumber),
		[3, 2, 1],
	);
	assert.deepEqual(
		newestDeploymentFirst([
			{ DeploymentNumber: 2 },
			{ DeploymentNumber: 10 },
			{},
		]).map((d) => d.DeploymentNumber),
		[10, 2, undefined],
	);
	assert.equal(versionText(3, "stable"), "3 (stable)");
	assert.equal(versionText("3", ""), "3");
	assert.equal(versionText(undefined), "-");
	assert.equal(versionText(""), "-");
});

test("a deployment needs a profile, a version and a strategy", () => {
	const form = {
		...emptyDeploymentForm(),
		profile: "prof1",
		version: "3",
		strategy: "AppConfig.AllAtOnce",
	};
	assert.deepEqual(startDeploymentInput("app1", "env1", form), {
		ApplicationId: "app1",
		EnvironmentId: "env1",
		ConfigurationProfileId: "prof1",
		ConfigurationVersion: "3",
		DeploymentStrategyId: "AppConfig.AllAtOnce",
	});
	assert.equal(
		startDeploymentInput("app1", "env1", { ...form, description: " go " })
			.Description,
		"go",
	);
	assert.throws(
		() => startDeploymentInput("a", "e", { ...form, profile: "" }),
		/configuration profile/,
	);
	assert.throws(
		() => startDeploymentInput("a", "e", { ...form, version: "" }),
		/hosted configuration version/,
	);
	assert.throws(
		() => startDeploymentInput("a", "e", { ...form, strategy: "" }),
		/deployment strategy/,
	);
});

test("deployment states map to status indicators and polling", () => {
	assert.equal(stateStatusType("COMPLETE"), "success");
	assert.equal(stateStatusType("READY_FOR_DEPLOYMENT"), "success");
	for (const state of ["BAKING", "VALIDATING", "DEPLOYING", "ROLLING_BACK"]) {
		assert.equal(stateStatusType(state), "in-progress", state);
		assert.equal(deploymentInProgress(state), true, state);
	}
	assert.equal(stateStatusType("ROLLED_BACK"), "error");
	assert.equal(stateStatusType("REVERTED"), "stopped");
	assert.equal(stateStatusType(undefined), "pending");
	for (const state of ["COMPLETE", "REVERTED", "ROLLED_BACK", undefined])
		assert.equal(deploymentInProgress(state), false, String(state));
	assert.equal(
		hasActiveDeployments([{ State: "COMPLETE" }, { State: "BAKING" }]),
		true,
	);
	assert.equal(
		hasActiveDeployments([{ State: "COMPLETE" }, { State: "REVERTED" }]),
		false,
	);
	assert.equal(hasActiveDeployments([]), false);
	assert.equal(percentText(100), "100%");
	assert.equal(percentText(33.333), "33.3%");
	assert.equal(percentText(undefined), "-");
});

test("only the latest completed deployment can be stopped, by reverting it", () => {
	const deployments = [
		{ DeploymentNumber: 3, State: "COMPLETE" },
		{ DeploymentNumber: 2, State: "COMPLETE" },
		{ DeploymentNumber: 1, State: "REVERTED" },
	];
	assert.equal(canStop(deployments[0], deployments), true);
	assert.equal(canStop(deployments[1], deployments), false);
	assert.equal(canStop(deployments[2], deployments), false);
	const reverted = [{ DeploymentNumber: 2, State: "REVERTED" }];
	assert.equal(canStop(reverted[0], reverted), false);
	assert.equal(canStop({ State: "COMPLETE" }, []), false);
	assert.deepEqual(stopDeploymentInput("app1", "env1", deployments[0]), {
		ApplicationId: "app1",
		EnvironmentId: "env1",
		DeploymentNumber: 3,
		AllowRevert: true,
	});
});

test("inventory records lead to the page that shows them", () => {
	const arn = (resource: string) =>
		`arn:aws:appconfig:eu-west-1:000000000000:${resource}`;
	assert.deepEqual(inventoryTarget(arn("application/app1234")), {
		application: "app1234",
		environment: undefined,
		profile: undefined,
	});
	assert.deepEqual(
		inventoryTarget(arn("application/app1234/environment/env1234")),
		{
			application: "app1234",
			environment: "env1234",
			profile: undefined,
		},
	);
	assert.deepEqual(
		inventoryTarget(
			arn("application/app1234/environment/env1234/deployment/7"),
		),
		{ application: "app1234", environment: "env1234", profile: undefined },
	);
	assert.deepEqual(
		inventoryTarget(arn("application/app1234/configurationprofile/prof123")),
		{ application: "app1234", environment: undefined, profile: "prof123" },
	);
	assert.deepEqual(
		inventoryTarget(
			arn(
				"application/app1234/configurationprofile/prof123/hostedconfigurationversion/2",
			),
		),
		{ application: "app1234", environment: undefined, profile: "prof123" },
	);
	assert.deepEqual(inventoryTarget(arn("deploymentstrategy/str1234")), {
		tab: "strategies",
	});
	assert.deepEqual(inventoryTarget(""), {
		application: undefined,
		environment: undefined,
		profile: undefined,
	});
});

test("deployment times show in the viewer's locale, and a dash when absent", () => {
	// Console actions give times with nine fractional digits.
	assert.equal(
		isoTime("2026-11-03T01:02:03.123456789Z"),
		new Date("2026-11-03T01:02:03.123Z").toLocaleString(),
	);
	assert.equal(isoTime(undefined), "-");
	assert.equal(isoTime(""), "-");
});
