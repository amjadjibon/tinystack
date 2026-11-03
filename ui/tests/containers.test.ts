import assert from "node:assert/strict";
import test from "node:test";
import {
	containerPages,
	desiredCount,
	taskDefinitionInput,
} from "../src/lib/containerHelpers.ts";

test("task definition registration rejects invalid containers and accepts revisions", () => {
	for (const v of [
		"[]",
		"{}",
		'{"family":"app","containerDefinitions":[]}',
		'{"family":"app","containerDefinitions":[{"name":"app","image":"a"}]}',
		'{"family":"app","containerDefinitions":[{"name":"app"}]}',
		'{"family":"app","containerDefinitions":[{"name":"app","image":"a","memory":128},{"name":"app","image":"b","memory":128}]}',
	])
		assert.throws(() => taskDefinitionInput(v));
	assert.equal(
		taskDefinitionInput(
			'{"family":"app","containerDefinitions":[{"name":"app","image":"local:latest","memory":128}]}',
		).family,
		"app",
	);
	assert.equal(
		taskDefinitionInput(
			'{"family":"app","memory":"512","containerDefinitions":[{"name":"app","image":"local:latest"}]}',
		).memory,
		"512",
	);
	assert.throws(
		() =>
			taskDefinitionInput(
				'{"family":"app","containerDefinitions":[{"name":"app","image":"local:latest","memory":64,"memoryReservation":128}]}',
			),
		/reservation/,
	);
	for (const v of ["", "-1", "1.5", "NaN"])
		assert.throws(() => desiredCount(v));
	assert.equal(desiredCount("0"), 0);
	assert.throws(() => desiredCount("0", 1));
	assert.throws(() => taskDefinitionInput("null"));
});
test("container listing collects pages and rejects repeated tokens", async () => {
	let n = 0;
	assert.deepEqual(
		await containerPages(
			async () =>
				++n === 1 ? { items: ["a"], NextToken: "next" } : { items: ["b"] },
			"List",
			"items",
		),
		["a", "b"],
	);
	await assert.rejects(
		containerPages(
			async () => ({ items: [], NextToken: "same" }),
			"List",
			"items",
		),
		/Repeated/,
	);
});
