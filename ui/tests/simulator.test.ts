import assert from "node:assert/strict";
import test from "node:test";
import {
	buildContextEntries,
	formatPolicyJson,
	isIamPrincipalArn,
	isIamUserOrRoleArn,
	parseActions,
	parseResources,
	validatePolicyJson,
} from "../src/lib/simulator.ts";

test("IAM principal ARN validation", () => {
	assert.equal(isIamPrincipalArn("arn:aws:iam::123456789012:user/alice"), true);
	assert.equal(
		isIamPrincipalArn("arn:aws:iam::123456789012:role/service-role/app"),
		true,
	);
	assert.equal(
		isIamPrincipalArn("arn:aws:iam::123456789012:group/developers"),
		true,
	);
	assert.equal(
		isIamPrincipalArn("arn:aws:sts::123456789012:assumed-role/r/s"),
		false,
	);
	assert.equal(isIamPrincipalArn("arn:aws:iam::123456789012:policy/p"), false);
	assert.equal(isIamPrincipalArn("invalid-arn"), false);

	assert.equal(
		isIamUserOrRoleArn("arn:aws:iam::123456789012:user/alice"),
		true,
	);
	assert.equal(
		isIamUserOrRoleArn("arn:aws:iam::123456789012:role/service-role/app"),
		true,
	);
	assert.equal(
		isIamUserOrRoleArn("arn:aws:iam::123456789012:group/developers"),
		false,
	);
});

test("parseActions enforces service:action without wildcards", () => {
	assert.deepEqual(parseActions("s3:GetObject, s3:PutObject"), {
		actions: ["s3:GetObject", "s3:PutObject"],
	});
	assert.deepEqual(parseActions("s3:GetObject\ns3:PutObject\n"), {
		actions: ["s3:GetObject", "s3:PutObject"],
	});
	assert.match(parseActions("").error ?? "", /At least one action is required/);
	assert.match(
		parseActions("s3:*").error ?? "",
		/Actions must use service:action format without wildcards/,
	);
	assert.match(
		parseActions("GetObject").error ?? "",
		/Actions must use service:action format without wildcards/,
	);
	assert.match(
		parseActions("s3:Get?bject").error ?? "",
		/Actions must use service:action format without wildcards/,
	);
});

test("parseResources defaults to wildcard * when empty", () => {
	assert.deepEqual(parseResources(""), ["*"]);
	assert.deepEqual(parseResources("   "), ["*"]);
	assert.deepEqual(
		parseResources("arn:aws:s3:::my-bucket, arn:aws:s3:::other"),
		["arn:aws:s3:::my-bucket", "arn:aws:s3:::other"],
	);
});

test("validatePolicyJson checks for valid JSON objects", () => {
	assert.equal(
		validatePolicyJson('{"Version": "2012-10-17", "Statement": []}'),
		null,
	);
	assert.equal(validatePolicyJson(""), null);
	assert.match(validatePolicyJson("{bad json") ?? "", /invalid JSON/);
	assert.match(validatePolicyJson("[]") ?? "", /must be a JSON object/);
	assert.match(validatePolicyJson('"string"') ?? "", /must be a JSON object/);
});

test("formatPolicyJson pretty-prints valid JSON or keeps raw", () => {
	assert.equal(formatPolicyJson('{"key":"value"}'), '{\n  "key": "value"\n}');
	assert.equal(formatPolicyJson("{bad"), "{bad");
});

test("buildContextEntries formats scalar and list types correctly", () => {
	const res = buildContextEntries([
		{
			id: "1",
			key: "aws:PrincipalTag/dept",
			type: "string",
			value: "engineering",
		},
		{
			id: "2",
			key: "aws:SourceIp",
			type: "ipList",
			value: "192.168.1.1, 10.0.0.1",
		},
		{ id: "3", key: "", type: "string", value: "" }, // Ignored empty row
	]);

	assert.equal(res.error, undefined);
	assert.deepEqual(res.entries, [
		{
			ContextKeyName: "aws:PrincipalTag/dept",
			ContextKeyType: "string",
			ContextKeyValues: ["engineering"],
		},
		{
			ContextKeyName: "aws:SourceIp",
			ContextKeyType: "ipList",
			ContextKeyValues: ["192.168.1.1", "10.0.0.1"],
		},
	]);

	// Missing value error
	const err = buildContextEntries([
		{ id: "1", key: "aws:CurrentTime", type: "date", value: "" },
	]);
	assert.match(err.error ?? "", /requires a value/);
});
