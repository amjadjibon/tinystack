import assert from "node:assert/strict";
import { test } from "node:test";
import {
	buildPublicPermissionInputs,
	buildRemovePermissionInputs,
	checkPublicPermissions,
	configToFormState,
	corsSummary,
	defaultFormState,
	extractQualifier,
	formStateToCreatePayload,
	formStateToUpdatePayload,
	PUBLIC_INVOKE_STATEMENT_ID,
	PUBLIC_URL_STATEMENT_ID,
	parseCommaList,
	parsePolicyStatements,
	validateForm,
} from "../src/lib/lambdaUrl.ts";

test("extractQualifier extracts qualifier from lambda ARN or defaults to $LATEST", () => {
	assert.equal(extractQualifier(undefined), "$LATEST");
	assert.equal(extractQualifier(""), "$LATEST");
	assert.equal(
		extractQualifier(
			"arn:aws:lambda:us-east-1:123456789012:function:my-function",
		),
		"$LATEST",
	);
	assert.equal(
		extractQualifier(
			"arn:aws:lambda:us-east-1:123456789012:function:my-function:$LATEST",
		),
		"$LATEST",
	);
	assert.equal(
		extractQualifier(
			"arn:aws:lambda:us-east-1:123456789012:function:my-function:prod",
		),
		"prod",
	);
	assert.equal(
		extractQualifier(
			"arn:aws:lambda:eu-west-1:999999999999:function:api-handler:staging",
		),
		"staging",
	);
});

test("parseCommaList trims and removes empty elements", () => {
	assert.deepEqual(parseCommaList("*"), ["*"]);
	assert.deepEqual(parseCommaList("https://a.com, https://b.com"), [
		"https://a.com",
		"https://b.com",
	]);
	assert.deepEqual(parseCommaList("  , a,  , b , "), ["a", "b"]);
	assert.deepEqual(parseCommaList(""), []);
});

test("parsePolicyStatements handles null, malformed JSON, and object/array formats", () => {
	assert.deepEqual(parsePolicyStatements(null), []);
	assert.deepEqual(parsePolicyStatements(undefined), []);
	assert.deepEqual(parsePolicyStatements(""), []);
	assert.deepEqual(parsePolicyStatements("not-json"), []);
	assert.deepEqual(parsePolicyStatements("123"), []);

	const arrayPolicy = JSON.stringify({
		Version: "2012-10-17",
		Statement: [
			{ Sid: "Statement1", Effect: "Allow" },
			{ Sid: "Statement2", Effect: "Allow" },
		],
	});
	assert.equal(parsePolicyStatements(arrayPolicy).length, 2);

	const singleObjPolicy = JSON.stringify({
		Version: "2012-10-17",
		Statement: { Sid: "SingleStatement", Effect: "Allow" },
	});
	assert.equal(parsePolicyStatements(singleObjPolicy).length, 1);
	assert.equal(
		parsePolicyStatements(singleObjPolicy)[0]?.Sid,
		"SingleStatement",
	);
});

test("checkPublicPermissions detects presence and absence of both required public statements", () => {
	// Missing policy entirely
	const empty = checkPublicPermissions(null);
	assert.equal(empty.hasUrlAccess, false);
	assert.equal(empty.hasInvokeAccess, false);
	assert.deepEqual(empty.missingSids, [
		PUBLIC_URL_STATEMENT_ID,
		PUBLIC_INVOKE_STATEMENT_ID,
	]);

	// Policy with only FunctionURLAllowPublicAccess
	const onlyUrl = JSON.stringify({
		Statement: [{ Sid: PUBLIC_URL_STATEMENT_ID, Effect: "Allow" }],
	});
	const onlyUrlRes = checkPublicPermissions(onlyUrl);
	assert.equal(onlyUrlRes.hasUrlAccess, true);
	assert.equal(onlyUrlRes.hasInvokeAccess, false);
	assert.deepEqual(onlyUrlRes.missingSids, [PUBLIC_INVOKE_STATEMENT_ID]);

	// Policy with only FunctionURLInvokeAllowPublicAccess
	const onlyInvoke = JSON.stringify({
		Statement: [{ Sid: PUBLIC_INVOKE_STATEMENT_ID, Effect: "Allow" }],
	});
	const onlyInvokeRes = checkPublicPermissions(onlyInvoke);
	assert.equal(onlyInvokeRes.hasUrlAccess, false);
	assert.equal(onlyInvokeRes.hasInvokeAccess, true);
	assert.deepEqual(onlyInvokeRes.missingSids, [PUBLIC_URL_STATEMENT_ID]);

	// Policy with both statements
	const both = JSON.stringify({
		Statement: [
			{ Sid: PUBLIC_URL_STATEMENT_ID, Effect: "Allow" },
			{ Sid: PUBLIC_INVOKE_STATEMENT_ID, Effect: "Allow" },
		],
	});
	const bothRes = checkPublicPermissions(both);
	assert.equal(bothRes.hasUrlAccess, true);
	assert.equal(bothRes.hasInvokeAccess, true);
	assert.deepEqual(bothRes.missingSids, []);
});

test("buildPublicPermissionInputs builds AddPermission requests with correct Sids, actions, and qualifiers", () => {
	// For $LATEST (unqualified)
	const latestInputs = buildPublicPermissionInputs("my-function", "$LATEST");
	assert.equal(latestInputs.length, 2);
	assert.deepEqual(latestInputs[0], {
		FunctionName: "my-function",
		StatementId: PUBLIC_URL_STATEMENT_ID,
		Action: "lambda:InvokeFunctionUrl",
		Principal: "*",
		FunctionUrlAuthType: "NONE",
	});
	assert.deepEqual(latestInputs[1], {
		FunctionName: "my-function",
		StatementId: PUBLIC_INVOKE_STATEMENT_ID,
		Action: "lambda:InvokeFunction",
		Principal: "*",
		InvokedViaFunctionUrl: true,
	});

	// For an alias
	const aliasInputs = buildPublicPermissionInputs("my-function", "prod");
	assert.equal(aliasInputs.length, 2);
	assert.equal(aliasInputs[0]?.Qualifier, "prod");
	assert.equal(aliasInputs[1]?.Qualifier, "prod");

	// When only one Sid needs adding
	const singleInput = buildPublicPermissionInputs("my-function", "prod", [
		PUBLIC_URL_STATEMENT_ID,
	]);
	assert.equal(singleInput.length, 1);
	assert.equal(singleInput[0]?.StatementId, PUBLIC_URL_STATEMENT_ID);
});

test("buildRemovePermissionInputs builds RemovePermission requests", () => {
	const latestRemovals = buildRemovePermissionInputs("my-function", "$LATEST");
	assert.deepEqual(latestRemovals, [
		{ FunctionName: "my-function", StatementId: PUBLIC_URL_STATEMENT_ID },
		{
			FunctionName: "my-function",
			StatementId: PUBLIC_INVOKE_STATEMENT_ID,
		},
	]);

	const aliasRemovals = buildRemovePermissionInputs("my-function", "prod");
	assert.deepEqual(aliasRemovals, [
		{
			FunctionName: "my-function",
			StatementId: PUBLIC_URL_STATEMENT_ID,
			Qualifier: "prod",
		},
		{
			FunctionName: "my-function",
			StatementId: PUBLIC_INVOKE_STATEMENT_ID,
			Qualifier: "prod",
		},
	]);
});

test("corsSummary provides readable summary for empty or configured CORS", () => {
	assert.equal(corsSummary(undefined), "Disabled");
	assert.equal(
		corsSummary({
			AllowOrigins: ["https://example.com"],
			AllowMethods: ["GET", "POST"],
			AllowHeaders: ["content-type"],
			AllowCredentials: true,
			MaxAge: 300,
		}),
		"Origins: https://example.com \u00b7 Methods: GET, POST \u00b7 Headers: content-type \u00b7 Credentials: yes \u00b7 Max age: 300s",
	);
});

test("validateForm validates alias name and CORS constraints", () => {
	const valid = defaultFormState();
	assert.equal(validateForm(valid), null);

	// Invalid alias name
	const invalidAlias = {
		...valid,
		qualifierType: "alias" as const,
		aliasName: "",
	};
	assert.match(validateForm(invalidAlias) ?? "", /Enter an alias name/);

	const reservedAlias = {
		...valid,
		qualifierType: "alias" as const,
		aliasName: "$LATEST",
	};
	assert.match(validateForm(reservedAlias) ?? "", /Select '\$LATEST' target/);

	// Invalid CORS maxAge
	const invalidCors = {
		...valid,
		enableCors: true,
		maxAge: "999999",
	};
	assert.match(
		validateForm(invalidCors) ?? "",
		/CORS MaxAge must be an integer from 0 to 86400/,
	);
});

test("formStateToCreatePayload and formStateToUpdatePayload construct valid API requests", () => {
	const form = defaultFormState();
	form.authType = "NONE";
	form.invokeMode = "BUFFERED";

	// Create for $LATEST
	const createLatest = formStateToCreatePayload(form, "my-func");
	assert.deepEqual(createLatest, {
		FunctionName: "my-func",
		AuthType: "NONE",
		InvokeMode: "BUFFERED",
	});

	// Create for alias with CORS
	form.qualifierType = "alias";
	form.aliasName = "live";
	form.enableCors = true;
	form.allowOrigins = "https://app.com";
	form.allowMethods = ["GET", "POST"];
	form.allowHeaders = "authorization";
	form.allowCredentials = true;
	form.maxAge = "600";

	const createAlias = formStateToCreatePayload(form, "my-func");
	assert.deepEqual(createAlias, {
		FunctionName: "my-func",
		Qualifier: "live",
		AuthType: "NONE",
		InvokeMode: "BUFFERED",
		Cors: {
			AllowOrigins: ["https://app.com"],
			AllowMethods: ["GET", "POST"],
			AllowHeaders: ["authorization"],
			AllowCredentials: true,
			MaxAge: 600,
		},
	});

	// Update payload
	const updateAlias = formStateToUpdatePayload(form, "my-func", "live");
	assert.deepEqual(updateAlias, {
		FunctionName: "my-func",
		Qualifier: "live",
		AuthType: "NONE",
		InvokeMode: "BUFFERED",
		Cors: {
			AllowOrigins: ["https://app.com"],
			AllowMethods: ["GET", "POST"],
			AllowHeaders: ["authorization"],
			AllowCredentials: true,
			MaxAge: 600,
		},
	});
});

test("configToFormState correctly populates form state from existing config", () => {
	const state = configToFormState(
		{
			AuthType: "AWS_IAM",
			InvokeMode: "RESPONSE_STREAM",
			Cors: {
				AllowOrigins: ["*"],
				AllowMethods: ["GET"],
				AllowHeaders: ["*"],
				AllowCredentials: true,
				MaxAge: 120,
			},
		},
		"staging",
	);

	assert.equal(state.qualifierType, "alias");
	assert.equal(state.aliasName, "staging");
	assert.equal(state.authType, "AWS_IAM");
	assert.equal(state.invokeMode, "RESPONSE_STREAM");
	assert.equal(state.enableCors, true);
	assert.equal(state.allowCredentials, true);
	assert.equal(state.maxAge, "120");
});
