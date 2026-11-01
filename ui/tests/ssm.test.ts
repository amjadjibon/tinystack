import assert from "node:assert/strict";
import test from "node:test";
import { parameterFilters, parameterMetadata } from "../src/lib/ssmHelpers.ts";

test("parameter metadata excludes values across ordinary and encrypted history without changing responses", () => {
	for (const Type of ["String", "StringList", "SecureString"]) {
		const item = {
			Name: "/app/key",
			Type,
			Version: 2,
			Value: "sensitive",
			Labels: ["stable"],
		};
		const result = parameterMetadata(item);
		assert.equal(Object.hasOwn(result, "Value"), false);
		assert.equal(item.Value, "sensitive");
		assert.equal(result.Version, 2);
		assert.deepEqual(result.Labels, ["stable"]);
	}
});
test("parameter filters distinguish name prefixes from one-level and recursive paths", () => {
	assert.deepEqual(parameterFilters("", "prefix", "", ""), []);
	for (const [mode, Key, Option] of [
		["prefix", "Name", "BeginsWith"],
		["OneLevel", "Path", "OneLevel"],
		["Recursive", "Path", "Recursive"],
	]) {
		assert.deepEqual(
			parameterFilters("/app", mode, "SecureString", "Advanced"),
			[
				{ Key, Option, Values: ["/app"] },
				{ Key: "Type", Option: "Equals", Values: ["SecureString"] },
				{ Key: "Tier", Option: "Equals", Values: ["Advanced"] },
			],
		);
	}
});
