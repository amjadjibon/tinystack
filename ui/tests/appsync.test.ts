import assert from "node:assert/strict";
import test from "node:test";
import {
	appsyncHref,
	decodeSDL,
	encodeSDL,
	formInput,
	initialForm,
	resourceIdentity,
	validateConfig,
} from "../src/lib/appsyncHelpers.ts";

test("AppSync forms keep only supported input members when editing", () => {
	const row = {
		Name: "api",
		ApiId: "id",
		Arn: "arn",
		AuthenticationType: "API_KEY",
		Uris: { GRAPHQL: "url" },
		Tags: { team: "dev" },
		XrayEnabled: true,
	};
	const input = formInput("api", initialForm("api", row));
	assert.equal(input.Name, "api");
	assert.equal(input.XrayEnabled, true);
	for (const field of ["ApiId", "Arn", "Uris", "Tags"])
		assert.equal(field in input, false);
	assert.deepEqual(resourceIdentity("api", row), { ApiId: "id" });
});
test("data source configs must match the supported type", () => {
	const f = initialForm("source", { Name: "source", Type: "AWS_LAMBDA" });
	assert.throws(() => formInput("source", f), /configuration/);
	f.LambdaConfig = '{"LambdaFunctionArn":"arn"}';
	assert.deepEqual(formInput("source", f).LambdaConfig, {
		LambdaFunctionArn: "arn",
	});
	f.HttpConfig = '{"Endpoint":"http://localhost"}';
	assert.throws(() => formInput("source", f), /other data source/);
	assert.throws(
		() => formInput("source", { ...f, Type: "AMAZON_RDS" }),
		/valid/,
	);
});
test("nested unsupported fields fail before sending", () => {
	assert.throws(
		() =>
			validateConfig(
				{ AuthorizationConfig: { AwsIamConfig: { Unknown: true } } },
				"HttpDataSourceConfig",
			),
		/Unsupported/,
	);
	assert.throws(
		() => validateConfig({}, "AdditionalAuthenticationProvider"),
		/array/,
	);
	validateConfig(
		[
			{
				AuthenticationType: "OPENID_CONNECT",
				OpenIDConnectConfig: { Issuer: "issuer", AuthTTL: 100 },
			},
		],
		"AdditionalAuthenticationProvider",
	);
	assert.throws(
		() =>
			validateConfig(
				[{ UserPoolConfig: { DefaultAction: "ALLOW" } }],
				"AdditionalAuthenticationProvider",
			),
		/Unsupported/,
	);
});
test("resolver JavaScript and VTL are exclusive; runtime is pinned", () => {
	const f = initialForm("resolver", {
		TypeName: "Query",
		FieldName: "hello",
		Runtime: { Name: "APPSYNC_JS", RuntimeVersion: "1.0.0" },
		Code: "export function request() { return {}; }",
	});
	assert.ok(formInput("resolver", f).Runtime);
	assert.throws(() => formInput("resolver", { ...f, Code: "" }), /together/);
	assert.throws(
		() => formInput("resolver", { ...f, RequestMappingTemplate: "{}" }),
		/or mapping/,
	);
	assert.throws(
		() => formInput("resolver", { ...f, MaxBatchSize: "2001" }),
		/2000/,
	);
	assert.throws(
		() =>
			formInput("resolver", {
				...f,
				Runtime: '{"Name":"APPSYNC_JS","RuntimeVersion":"2"}',
			}),
		/1.0.0/,
	);
});
test("expiry is an integer and identities preserve resolver and function selectors", () => {
	assert.throws(() => formInput("key", { Expires: "NaN" }), /integer/);
	assert.equal(formInput("key", { Expires: "1800000000" }).Expires, 1800000000);
	assert.deepEqual(
		resourceIdentity("resolver", { TypeName: "Query", FieldName: "hello" }),
		{ TypeName: "Query", FieldName: "hello" },
	);
	assert.deepEqual(
		resourceIdentity("function", { FunctionId: "fn", Name: "name" }),
		{ FunctionId: "fn" },
	);
});
test("SDL handles Unicode bytes and API selection is escaped", () => {
	const sdl = '"""日本語""" type Query { hello: String }';
	assert.equal(decodeSDL(encodeSDL(sdl)), sdl);
	assert.throws(() => encodeSDL(" "), /Enter a schema/);
	assert.equal(
		new URL(
			appsyncHref({ account: "000000000000", region: "eu-west-1" }, "a&b"),
			"http://localhost",
		).searchParams.get("appsyncApi"),
		"a&b",
	);
});
