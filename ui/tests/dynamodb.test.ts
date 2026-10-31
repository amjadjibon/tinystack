import assert from "node:assert/strict";
import test from "node:test";
import {
	batchChunks,
	dynamoName,
	statementInput,
} from "../src/lib/dynamodbHelpers.ts";

test("batch writes split at 25 without losing or repeating items", () => {
	const rows = Array.from({ length: 51 }, (_, i) => i);
	const batches = batchChunks(rows);
	assert.deepEqual(
		batches.map((b) => b.length),
		[25, 25, 1],
	);
	assert.deepEqual(batches.flat(), rows);
	assert.deepEqual(batchChunks([]), []);
});
test("backup and restore names reject invalid names", () => {
	for (const name of ["ab", "a/b", "x".repeat(256), ""])
		assert.throws(() => dynamoName(name));
	assert.equal(dynamoName("table-123.test"), "table-123.test");
});
test("PartiQL preserves numeric and binary parameters and enforces page limits", () => {
	assert.deepEqual(
		statementInput(
			"SELECT * FROM test WHERE pk=?",
			'[{"N":"9007199254740993"},{"B":"AQ=="}]',
			"25",
		),
		{
			Statement: "SELECT * FROM test WHERE pk=?",
			Parameters: [{ N: "9007199254740993" }, { B: "AQ==" }],
			Limit: 25,
		},
	);
	for (const limit of ["0", "1001", "1.5", "NaN"])
		assert.throws(() => statementInput("SELECT * FROM test", "[]", limit));
	assert.throws(() => statementInput("", "[]", "25"));
	assert.throws(() => statementInput("SELECT * FROM test", "{}", "25"));
});
