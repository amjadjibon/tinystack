import assert from "node:assert/strict";
import test from "node:test";
import {
	eventJSON,
	lookupAttributes,
	lookupInput,
	selectorInput,
	trailForm,
	trailInput,
} from "../src/lib/cloudtrailHelpers.ts";

test("trail forms send only supported create and update members", () => {
	const form = {
		...trailForm(),
		name: "audit-trail",
		bucket: "existing",
		prefix: "logs/",
		tags: '{"env":"dev"}',
	};
	assert.deepEqual(trailInput(form, true), {
		Name: "audit-trail",
		S3BucketName: "existing",
		S3KeyPrefix: "logs/",
		TagsList: [{ Key: "env", Value: "dev" }],
	});
	assert.deepEqual(
		trailInput({ ...form, prefix: "", tags: "invalid" }, false),
		{ Name: "audit-trail", S3BucketName: "existing", S3KeyPrefix: "" },
	);
	assert.equal(
		trailForm({ Name: "audit-trail", S3BucketName: "existing" }).prefix,
		"",
	);
	for (const patch of [
		{ name: "ab" },
		{ name: "bad/name" },
		{ bucket: "" },
		{ tags: '{"a":1}' },
		{ tags: "[]" },
	])
		assert.throws(() => trailInput({ ...form, ...patch }, true));
});
test("lookup filters use seconds, one supported attribute, and inclusive ranges", () => {
	const f = { attribute: "", value: "", start: "", end: "" };
	assert.deepEqual(lookupInput(f), { MaxResults: 50 });
	for (const attribute of lookupAttributes)
		assert.deepEqual(
			lookupInput({
				...f,
				attribute,
				value: attribute === "ReadOnly" ? "false" : "value",
			}).LookupAttributes,
			[
				{
					AttributeKey: attribute,
					AttributeValue: attribute === "ReadOnly" ? "false" : "value",
				},
			],
		);
	const times = lookupInput({
		...f,
		start: "2026-01-01T00:00:00Z",
		end: "2026-01-01T00:00:00Z",
	});
	assert.equal(times.StartTime, 1767225600);
	assert.equal(times.EndTime, times.StartTime);
	for (const patch of [
		{ attribute: "Unknown", value: "x" },
		{ attribute: "EventName" },
		{ attribute: "ReadOnly", value: "yes" },
		{ start: "invalid" },
		{ start: "2026-02-01", end: "2026-01-01" },
	])
		assert.throws(() => lookupInput({ ...f, ...patch }));
});
test("event JSON expands safely and selector modes are mutually exclusive", () => {
	assert.equal(eventJSON(), "-");
	assert.equal(eventJSON("oops"), "oops");
	assert.equal(eventJSON('{"a":1}'), '{\n  "a": 1\n}');
	for (const value of [
		{ EventSelectors: [{ ReadWriteType: "All" }] },
		{
			AdvancedEventSelectors: [
				{
					FieldSelectors: [{ Field: "eventCategory", Equals: ["Management"] }],
				},
			],
		},
	])
		assert.deepEqual(selectorInput(JSON.stringify(value)), value);
	for (const value of [
		{},
		{ EventSelectors: [], AdvancedEventSelectors: [] },
		{ EventSelectors: [{}], AdvancedEventSelectors: [{}] },
		{ InsightSelectors: [{}] },
		{ EventSelectors: [{ Unsupported: true }] },
		{ AdvancedEventSelectors: [{ FieldSelectors: [] }] },
		{
			AdvancedEventSelectors: [
				{ FieldSelectors: [{ Field: "eventCategory" }] },
			],
		},
		{ EventSelectors: Array(6).fill({}) },
	])
		assert.throws(() => selectorInput(JSON.stringify(value)));
});
