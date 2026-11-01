import assert from "node:assert/strict";
import test from "node:test";
import {
	finalSnapshotInput,
	hasPendingSnapshots,
	validateName,
	validatePassword,
} from "../src/lib/rdsHelpers.ts";

test("final snapshots require a valid identifier and skip omits it", () => {
	assert.deepEqual(finalSnapshotInput(true, "stale-value"), {
		SkipFinalSnapshot: true,
	});
	assert.deepEqual(finalSnapshotInput(false, "final-snapshot"), {
		SkipFinalSnapshot: false,
		FinalDBSnapshotIdentifier: "final-snapshot",
	});
	for (const id of ["", "bad--name", "bad-", "1bad", "a".repeat(256)])
		assert.throws(() => finalSnapshotInput(false, id));
	assert.doesNotThrow(() => finalSnapshotInput(false, "a".repeat(255)));
	assert.throws(() => validateName("a".repeat(64)));
});
test("instance passwords reject unsupported characters and lengths", () => {
	for (const password of [
		"short",
		"a".repeat(42),
		"password/",
		"password@",
		'password"',
		"password ",
	])
		assert.throws(() => validatePassword(password));
	assert.doesNotThrow(() => validatePassword("console-password"));
});
test("snapshot lists poll only while a snapshot is not settled", () => {
	assert.equal(hasPendingSnapshots([]), false);
	assert.equal(
		hasPendingSnapshots([{ Status: "available" }, { Status: "failed" }]),
		false,
	);
	assert.equal(
		hasPendingSnapshots([{ Status: "available" }, { Status: "creating" }]),
		true,
	);
	assert.equal(hasPendingSnapshots([{ Status: "copying" }]), true);
});
