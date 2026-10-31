import assert from "node:assert/strict";
import test from "node:test";
import { changedAttributes, matchesUser } from "../src/lib/cognitoHelpers.ts";

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
