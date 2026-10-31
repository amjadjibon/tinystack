import assert from "node:assert/strict";
import test from "node:test";
import {
	changedAttributes,
	clientUpdateKeys,
	editableSettings,
	formInput,
	matchesUser,
	poolUpdateKeys,
	removedAttributes,
} from "../src/lib/cognitoHelpers.ts";

test("Cognito search matches username, email and status, ignoring case and whitespace", () => {
	const u = {
		Username: "Alice",
		UserStatus: "FORCE_CHANGE_PASSWORD",
		Attributes: [
			{ Name: "email", Value: "alice@example.com" },
			{ Name: "phone_number", Value: "+123" },
		],
	};
	for (const query of ["", " ALICE ", "example.com", "force_change"])
		assert.equal(matchesUser(u, query), true);
	for (const query of ["bob", "+123"])
		assert.equal(matchesUser(u, query), false);
	assert.equal(matchesUser({ Username: "bob" }, "CONFIRMED"), false);
});
test("Attribute updates include additions and changes while preserving unchanged values", () => {
	assert.deepEqual(
		changedAttributes(
			[
				{ Name: "email", Value: "a@b" },
				{ Name: "name", Value: "old" },
			],
			{ email: "a@b", name: "new", "custom:team": "dev" },
		),
		[
			{ Name: "name", Value: "new" },
			{ Name: "custom:team", Value: "dev" },
		],
	);
});

test("Attribute removals protect sub and distinguish absent from empty values", () => {
	const before = [
		{ Name: "sub", Value: "id" },
		{ Name: "email", Value: "a@b" },
		{ Name: "name", Value: "Alice" },
	];
	assert.deepEqual(removedAttributes(before, { email: "" }), ["name"]);
	assert.throws(
		() => removedAttributes(before, { sub: "changed" }),
		/read-only/,
	);
});
test("Updates preserve supported settings while excluding describe-only fields and secrets", () => {
	assert.deepEqual(
		editableSettings(
			{
				Name: "pool",
				Id: "id",
				Policies: { PasswordPolicy: { MinimumLength: 12 } },
				LambdaConfig: { PreSignUp: "arn" },
			},
			poolUpdateKeys,
		),
		{
			Policies: { PasswordPolicy: { MinimumLength: 12 } },
			LambdaConfig: { PreSignUp: "arn" },
		},
	);
	assert.deepEqual(
		editableSettings(
			{
				ClientId: "id",
				ClientName: "app",
				ClientSecret: "secret",
				CreationDate: 1,
				RefreshTokenRotation: { Feature: "ENABLED" },
			},
			clientUpdateKeys,
		),
		{
			ClientId: "id",
			ClientName: "app",
			RefreshTokenRotation: { Feature: "ENABLED" },
		},
	);
});
test("Forms parse nested policy edits, explicit false, empty lists, and token validity without mutating base", () => {
	const base = {
		Policies: { PasswordPolicy: { RequireSymbols: true, MinimumLength: 8 } },
	};
	const fields = [
		{
			key: "Policies.PasswordPolicy.MinimumLength",
			label: "Length",
			kind: "number" as const,
			min: 6,
			max: 99,
		},
		{
			key: "Policies.PasswordPolicy.RequireSymbols",
			label: "Symbols",
			kind: "boolean" as const,
		},
		{ key: "CallbackURLs", label: "URLs", kind: "list" as const },
	];
	assert.deepEqual(
		formInput(base, fields, {
			"Policies.PasswordPolicy.MinimumLength": "12",
			"Policies.PasswordPolicy.RequireSymbols": "false",
			CallbackURLs: "",
		}),
		{
			Policies: {
				PasswordPolicy: { MinimumLength: 12, RequireSymbols: false },
			},
			CallbackURLs: [],
		},
	);
	assert.equal(base.Policies.PasswordPolicy.MinimumLength, 8);
	for (const v of ["5", "100", "NaN", "1.5", "-1"])
		assert.throws(() =>
			formInput({}, fields, { "Policies.PasswordPolicy.MinimumLength": v }),
		);
	assert.throws(() =>
		formInput({}, [{ key: "Name", label: "Name", required: true }], {}),
	);
	assert.deepEqual(
		formInput({}, [{ key: "Auth", label: "Flows", kind: "list" }], {
			Auth: "a, b\na",
		}),
		{ Auth: ["a", "b"] },
	);
});

test("Blank optional nested units and JSON do not send invalid empty values", () => {
	assert.deepEqual(
		formInput(
			{},
			[
				{
					key: "TokenValidityUnits.AccessToken",
					label: "Unit",
					options: ["hours"],
				},
				{ key: "SmsConfiguration", label: "SMS", kind: "json" },
			],
			{},
		),
		{ TokenValidityUnits: {} },
	);
});
