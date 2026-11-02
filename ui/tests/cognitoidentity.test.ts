import assert from "node:assert/strict";
import test from "node:test";
import {
	chunks,
	emptyPoolForm,
	emptyProviderRow,
	formFromPool,
	formFromRoles,
	type IdentityPool,
	identityKind,
	mappingKeys,
	type PoolForm,
	parseMappings,
	poolArn,
	poolInput,
	providerEntries,
	providerSummary,
	roleName,
	rolesInput,
	ruleText,
	userPoolProvider,
	userPoolRef,
} from "../src/lib/cognitoidentityHelpers.ts";

const provider = "cognito-idp.us-east-1.amazonaws.com/us-east-1_AbCdEf123";
const oidc = "arn:aws:iam::000000000000:oidc-provider/idp.example.com";
const role = "arn:aws:iam::000000000000:role/app";
const form: PoolForm = { ...emptyPoolForm(), name: "app users" };
const pool: IdentityPool = {
	IdentityPoolId: "us-east-1:00000000-0000-0000-0000-000000000000",
	IdentityPoolName: "app users",
	AllowUnauthenticatedIdentities: true,
	AllowClassicFlow: true,
	DeveloperProviderName: "login.acme",
	SupportedLoginProviders: { "accounts.google.com": "google-app" },
	OpenIdConnectProviderARNs: [oidc],
	CognitoIdentityProviders: [
		{ ProviderName: provider, ClientId: "client", ServerSideTokenCheck: true },
	],
	IdentityPoolTags: { env: "dev" },
};
const rule = {
	Claim: "cognito:groups",
	MatchType: "Contains",
	Value: "admins",
	RoleARN: role,
};

test("a new pool carries only what the form sets", () => {
	assert.deepEqual(poolInput({ ...form, name: "  app users  " }), {
		IdentityPoolName: "app users",
		AllowUnauthenticatedIdentities: false,
		AllowClassicFlow: false,
	});
	const input = poolInput({
		...form,
		allowUnauthenticated: true,
		allowClassic: true,
		developerProvider: " login.acme ",
		userPools: [
			{
				...emptyProviderRow(),
				name: ` ${provider} `,
				clientId: " client ",
				serverSideCheck: true,
			},
			{ ...emptyProviderRow(), name: provider },
		],
		social: { "accounts.google.com": " google-app ", "graph.facebook.com": "" },
		oidc: ` ${oidc}\n\n${oidc}\n`,
		tags: '{"env":"dev"}',
	});
	assert.deepEqual(input, {
		IdentityPoolName: "app users",
		AllowUnauthenticatedIdentities: true,
		AllowClassicFlow: true,
		DeveloperProviderName: "login.acme",
		CognitoIdentityProviders: [
			{
				ProviderName: provider,
				ClientId: "client",
				ServerSideTokenCheck: true,
			},
			{ ProviderName: provider },
		],
		SupportedLoginProviders: { "accounts.google.com": "google-app" },
		OpenIdConnectProviderARNs: [oidc],
		IdentityPoolTags: { env: "dev" },
	});
});

test("an update replaces the settings, names the pool and leaves tags alone", () => {
	const input = poolInput(
		{ ...formFromPool(pool), tags: '{"ignored":"yes"}' },
		pool.IdentityPoolId,
	);
	assert.equal(input.IdentityPoolId, pool.IdentityPoolId);
	assert.equal(Object.hasOwn(input, "IdentityPoolTags"), false);
	assert.deepEqual(input.CognitoIdentityProviders, [
		{ ProviderName: provider, ClientId: "client", ServerSideTokenCheck: true },
	]);
	assert.equal(input.DeveloperProviderName, "login.acme");
	// Removing everything clears it, because an update replaces the whole pool.
	const cleared = poolInput(
		{ ...formFromPool(pool), userPools: [], social: {}, oidc: "" },
		pool.IdentityPoolId,
	);
	for (const key of [
		"CognitoIdentityProviders",
		"SupportedLoginProviders",
		"OpenIdConnectProviderARNs",
	])
		assert.equal(Object.hasOwn(cleared, key), false);
});

test("editing a pool starts from its settings without sharing them", () => {
	const edit = formFromPool(pool);
	assert.equal(edit.name, "app users");
	assert.equal(edit.allowClassic, true);
	assert.equal(edit.oidc, oidc);
	assert.deepEqual(
		edit.userPools.map((r) => r.name),
		[provider],
	);
	assert.equal(edit.userPools[0].serverSideCheck, true);
	edit.social["accounts.google.com"] = "changed";
	assert.equal(
		pool.SupportedLoginProviders?.["accounts.google.com"],
		"google-app",
	);
	const ids = new Set(
		[
			...formFromPool(pool).userPools,
			...formFromPool(pool).userPools,
			emptyProviderRow(),
		].map((r) => r.id),
	);
	assert.equal(ids.size, 3);
});

test("pool settings that the service refuses fail before submission", () => {
	const refused: [Partial<PoolForm>, RegExp][] = [
		[{ name: "" }, /Name must be 1 to 128/],
		[{ name: "bad/name" }, /Name must be 1 to 128/],
		[{ name: "x".repeat(129) }, /Name must be 1 to 128/],
		[{ developerProvider: "bad name" }, /developer provider name/],
		[
			{ userPools: [{ ...emptyProviderRow(), name: "" }] },
			/must look like cognito-idp/,
		],
		[
			{ userPools: [{ ...emptyProviderRow(), name: "us-east-1_AbCdEf123" }] },
			/must look like cognito-idp/,
		],
		[{ social: { "example.com": "id" } }, /not a supported login provider/],
		[
			{ oidc: "arn:aws:iam::000000000000:role/app" },
			/OpenID Connect provider ARN/,
		],
		[{ oidc: "idp.example.com" }, /OpenID Connect provider ARN/],
		[{ tags: "[]" }, /Tags must be a JSON object/],
		[{ tags: '{"env":1}' }, /Tags values must be strings/],
		[{ tags: "{" }, /Tags must be valid JSON/],
	];
	for (const [patch, reason] of refused)
		assert.throws(
			() => poolInput({ ...form, ...patch }),
			reason,
			JSON.stringify(patch),
		);
});

test("user pool providers and ARNs are built and read back", () => {
	assert.equal(userPoolProvider("us-east-1", "us-east-1_AbCdEf123"), provider);
	assert.deepEqual(userPoolRef(provider), {
		region: "us-east-1",
		id: "us-east-1_AbCdEf123",
	});
	assert.equal(userPoolRef("accounts.google.com"), undefined);
	assert.equal(
		poolArn("000000000000", "us-east-1", pool.IdentityPoolId),
		`arn:aws:cognito-identity:us-east-1:000000000000:identitypool/${pool.IdentityPoolId}`,
	);
});

test("providers are listed by kind and summarised", () => {
	assert.deepEqual(
		providerEntries(pool).map((e) => [e.kind, e.name, e.detail]),
		[
			[
				"Cognito user pool",
				provider,
				"App client client, server-side token check",
			],
			["OpenID Connect", oidc, "-"],
			["Social login", "accounts.google.com", "App ID google-app"],
			["Developer", "login.acme", "-"],
		],
	);
	assert.equal(
		providerSummary(pool),
		"Cognito user pool, OpenID Connect, Social login, Developer",
	);
	assert.equal(
		providerSummary({
			...pool,
			CognitoIdentityProviders: [
				{ ProviderName: provider },
				{ ProviderName: provider, ClientId: "other" },
			],
		}).split(", ")[0],
		"Cognito user pool (2)",
	);
	assert.equal(
		providerSummary({
			...pool,
			DeveloperProviderName: undefined,
			SupportedLoginProviders: {},
			OpenIdConnectProviderARNs: [],
			CognitoIdentityProviders: [],
		}),
		"-",
	);
	assert.equal(
		providerEntries({
			...pool,
			CognitoIdentityProviders: [{ ProviderName: provider }],
		})[0].detail,
		"Any app client",
	);
});

test("roles keep the two role kinds and validate their mappings", () => {
	assert.deepEqual(
		rolesInput("pool", {
			authenticated: "",
			unauthenticated: "",
			mappings: "",
		}),
		{ IdentityPoolId: "pool", Roles: {} },
	);
	const mappings = {
		[`${provider}:client`]: {
			Type: "Rules",
			AmbiguousRoleResolution: "Deny",
			RulesConfiguration: { Rules: [rule] },
		},
		"idp.example.com": {
			Type: "Token",
			AmbiguousRoleResolution: "AuthenticatedRole",
		},
	};
	assert.deepEqual(
		rolesInput("pool", {
			authenticated: ` ${role} `,
			unauthenticated: "arn:aws:iam::000000000000:role/guest",
			mappings: JSON.stringify(mappings),
		}),
		{
			IdentityPoolId: "pool",
			Roles: {
				authenticated: role,
				unauthenticated: "arn:aws:iam::000000000000:role/guest",
			},
			RoleMappings: mappings,
		},
	);
});

test("roles that the service refuses fail before submission", () => {
	const mapping = (patch: Record<string, unknown>) =>
		JSON.stringify({
			key: {
				Type: "Rules",
				AmbiguousRoleResolution: "Deny",
				RulesConfiguration: { Rules: [rule] },
				...patch,
			},
		});
	const roles = { authenticated: "", unauthenticated: "", mappings: "{}" };
	assert.throws(
		() => rolesInput("pool", { ...roles, authenticated: "app" }),
		/authenticated role/,
	);
	assert.throws(
		() =>
			rolesInput("pool", {
				...roles,
				unauthenticated: "arn:aws:iam::0:user/a",
			}),
		/unauthenticated role/,
	);
	const rules = (patch: Record<string, unknown>) =>
		mapping({ RulesConfiguration: { Rules: [{ ...rule, ...patch }] } });
	for (const [mappings, reason] of [
		["[]", /must be a JSON object/],
		["{", /valid JSON/],
		['{"": {}}', /keys must be 1 to 128/],
		[JSON.stringify({ ["k".repeat(129)]: {} }), /keys must be 1 to 128/],
		['{"key": []}', /must be a JSON object/],
		[mapping({ Type: "Other" }), /Type must be Token or Rules/],
		[
			mapping({ AmbiguousRoleResolution: undefined }),
			/AuthenticatedRole or Deny/,
		],
		[
			mapping({ AmbiguousRoleResolution: "Allow" }),
			/AuthenticatedRole or Deny/,
		],
		[mapping({ RulesConfiguration: undefined }), /required for Rules/],
		[mapping({ RulesConfiguration: { Rules: [] } }), /1 to 25 rules/],
		[
			mapping({ RulesConfiguration: { Rules: Array(26).fill(rule) } }),
			/1 to 25 rules/,
		],
		[mapping({ RulesConfiguration: { Other: [] } }), /Other is not supported/],
		[rules({ MatchType: "Regex" }), /MatchType must be Equals/],
		[rules({ Claim: "" }), /Claim is required/],
		[rules({ Value: undefined }), /Value is required/],
		[rules({ RoleARN: "role" }), /RoleARN must be an IAM role ARN/],
		[rules({ Extra: 1 }), /Extra is not supported/],
		[mapping({ Extra: 1 }), /Extra is not supported/],
		[mapping({ Type: "Token" }), /applies only to Rules/],
	] as const)
		assert.throws(() => parseMappings(mappings), reason, mappings);
	assert.equal(Object.keys(parseMappings(mapping({}))).length, 1);
});

test("the roles form starts from the pool's roles", () => {
	const mappings = {
		key: { Type: "Token", AmbiguousRoleResolution: "Deny" },
	};
	const edit = formFromRoles({
		Roles: { authenticated: role },
		RoleMappings: mappings,
	});
	assert.equal(edit.authenticated, role);
	assert.equal(edit.unauthenticated, "");
	assert.deepEqual(parseMappings(edit.mappings), mappings);
	assert.deepEqual(parseMappings(formFromRoles({}).mappings), {});
});

test("role mapping keys name the pool's providers", () => {
	assert.deepEqual(mappingKeys(pool), [
		`${provider}:client`,
		"idp.example.com",
	]);
	assert.deepEqual(
		mappingKeys({
			...pool,
			CognitoIdentityProviders: [{ ProviderName: provider }],
		}),
		[provider, "idp.example.com"],
	);
	assert.deepEqual(
		mappingKeys({
			...pool,
			CognitoIdentityProviders: [],
			OpenIdConnectProviderARNs: [],
		}),
		[],
	);
});

test("rules and identities read plainly", () => {
	assert.equal(roleName(role), "app");
	assert.equal(roleName("arn:aws:iam::000000000000:role/path/app"), "path/app");
	assert.equal(ruleText(rule), 'cognito:groups Contains "admins" → app');
	assert.equal(identityKind({ IdentityId: "a" }), "Unauthenticated");
	assert.equal(
		identityKind({ IdentityId: "a", Logins: [] }),
		"Unauthenticated",
	);
	assert.equal(
		identityKind({ IdentityId: "a", Logins: [provider] }),
		"Authenticated",
	);
});

test("identities are deleted in batches the service accepts", () => {
	const ids = Array.from({ length: 125 }, (_, i) => `id-${i}`);
	assert.deepEqual(
		chunks(ids, 60).map((c) => c.length),
		[60, 60, 5],
	);
	assert.deepEqual(chunks([], 60), []);
	assert.deepEqual(chunks(["a"], 60), [["a"]]);
});
