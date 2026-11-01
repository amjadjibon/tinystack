import assert from "node:assert/strict";
import test from "node:test";
import {
	decodeRecord,
	encodeRecord,
	integer,
	type StreamCall,
	streamNames,
	streamShards,
	streamTags,
} from "../src/lib/streamingHelpers.ts";

test("record tools preserve UTF-8 bytes and retain binary base64", () => {
	const text = "Hello 世界 🚀\n";
	assert.equal(decodeRecord(encodeRecord(text)), text);
	assert.equal(decodeRecord("/wA="), "[Binary data, base64] /wA=");
	assert.throws(() => encodeRecord(""), /Enter record/);
	assert.throws(() => integer("", "Shards", 1, 10000));
	assert.throws(() => integer("1.5", "Shards", 1, 10000));
});
test("stream lists collect pages before displaying resources", async () => {
	for (const service of ["kinesis", "firehose"] as const) {
		const calls: Record<string, unknown>[] = [];
		const call: StreamCall = async <T>(
			_op: string,
			input: Record<string, unknown> = {},
		) => {
			calls.push(input);
			return (
				service === "kinesis"
					? {
							StreamNames: calls.length === 1 ? ["a"] : ["b"],
							HasMoreStreams: calls.length === 1,
						}
					: {
							DeliveryStreamNames: calls.length === 1 ? ["a"] : ["b"],
							HasMoreDeliveryStreams: calls.length === 1,
						}
			) as T;
		};
		assert.deepEqual(await streamNames(call, service), ["a", "b"]);
		assert.equal(
			calls[1][
				service === "kinesis"
					? "ExclusiveStartStreamName"
					: "ExclusiveStartDeliveryStreamName"
			],
			"a",
		);
	}
});
test("pagination rejects repeated cursors instead of looping", async () => {
	const call: StreamCall = async <T>() =>
		({ StreamNames: ["a"], HasMoreStreams: true }) as T;
	await assert.rejects(streamNames(call, "kinesis"), /invalid stream page/);
	const shards: StreamCall = async <T>() =>
		({ Shards: [{ ShardId: "one" }], NextToken: "same" }) as T;
	await assert.rejects(streamShards(shards), /repeated shard token/);
});
test("tag and shard pages use their service cursors", async () => {
	let count = 0;
	const tags: StreamCall = async <T>(
		_op: string,
		input: Record<string, unknown> = {},
	) => {
		count++;
		if (count === 2) assert.equal(input.ExclusiveStartTagKey, "a");
		return {
			Tags: [{ Key: count === 1 ? "a" : "b", Value: "value" }],
			HasMoreTags: count === 1,
		} as T;
	};
	assert.deepEqual(await streamTags(tags, "firehose"), {
		a: "value",
		b: "value",
	});
	count = 0;
	const shards: StreamCall = async <T>(
		_op: string,
		input: Record<string, unknown> = {},
	) => {
		count++;
		if (count === 2) {
			assert.equal(input.NextToken, "next");
			assert.equal(input.StreamName, undefined);
		}
		return {
			Shards: [{ ShardId: String(count) }],
			...(count === 1 ? { NextToken: "next" } : {}),
		} as T;
	};
	assert.deepEqual(
		(await streamShards(shards)).map((s) => s.ShardId),
		["1", "2"],
	);
});

test("tag keys that match Object properties remain editable", async () => {
	const call: StreamCall = async <T>() =>
		({
			Tags: [
				{ Key: "__proto__", Value: "team" },
				{ Key: "constructor", Value: "stream" },
			],
		}) as T;
	const tags = await streamTags(call, "kinesis");
	assert.equal(Object.hasOwn(tags, "__proto__"), true);
	assert.equal(
		Object.getOwnPropertyDescriptor(tags, "__proto__")?.value,
		"team",
	);
	assert.equal(tags.constructor, "stream");
});
