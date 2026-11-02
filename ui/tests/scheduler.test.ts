import assert from "node:assert/strict";
import test from "node:test";
import {
	atExpression,
	cronExpression,
	emptyScheduleForm,
	formFromSchedule,
	parseDate,
	rateExpression,
	type ScheduleForm,
	scheduleInput,
	splitExpression,
	targetOptions,
	targetService,
} from "../src/lib/schedulerHelpers.ts";

const queue = "arn:aws:sqs:us-east-1:000000000000:jobs";
const role = "arn:aws:iam::000000000000:role/scheduler";

const form: ScheduleForm = {
	...emptyScheduleForm(),
	name: "nightly",
	targetArn: queue,
	roleArn: role,
};

test("rate expressions use the singular unit for one", () => {
	assert.equal(rateExpression("1", "hour"), "rate(1 hour)");
	assert.equal(rateExpression("5", "minute"), "rate(5 minutes)");
	for (const bad of ["", "0", "-1", "1.5", "x"])
		assert.throws(() => rateExpression(bad, "day"));
});

test("cron and one-time expressions are validated and normalised", () => {
	assert.equal(cronExpression(" 0  12 * * ? * "), "cron(0 12 * * ? *)");
	assert.equal(cronExpression("cron(0 12 * * ? *)"), "cron(0 12 * * ? *)");
	assert.throws(() => cronExpression("0 12 * * ?"));
	assert.equal(atExpression("2030-01-31T09:30:00"), "at(2030-01-31T09:30:00)");
	assert.throws(() => atExpression("2030-01-31"));
	assert.throws(() => atExpression("2030-13-31T09:30:00"));
});

test("a stored expression splits back into its fields", () => {
	assert.deepEqual(splitExpression("rate(1 hour)"), {
		kind: "rate",
		rateValue: "1",
		rateUnit: "hour",
		cron: "",
		at: "",
	});
	assert.equal(splitExpression("cron(0 12 * * ? *)").cron, "0 12 * * ? *");
	assert.equal(splitExpression("at(2030-01-31T09:30:00)").kind, "at");
});

test("dates are read as UTC unless they carry an offset", () => {
	assert.equal(parseDate("", "Start"), undefined);
	assert.equal(parseDate("2030-01-01T00:00:00Z", "Start"), 1893456000);
	assert.equal(parseDate("2030-01-01T00:00:00", "Start"), 1893456000);
	assert.equal(parseDate("2030-01-01T01:00:00+01:00", "Start"), 1893456000);
	assert.throws(() => parseDate("soon", "Start"));
});

test("schedule input carries the full definition", () => {
	assert.deepEqual(scheduleInput(form), {
		Name: "nightly",
		GroupName: "default",
		ScheduleExpression: "rate(5 minutes)",
		State: "ENABLED",
		ActionAfterCompletion: "NONE",
		ScheduleExpressionTimezone: "UTC",
		FlexibleTimeWindow: { Mode: "OFF" },
		Target: { Arn: queue, RoleArn: role },
	});
	const full = scheduleInput({
		...form,
		kind: "cron",
		cron: "0 12 * * ? *",
		timezone: "Europe/Berlin",
		window: "FLEXIBLE",
		windowMinutes: "15",
		end: "2031-01-01T00:00:00Z",
		input: '{"a":1}',
		retryAge: "120",
		retryAttempts: "3",
		dlqArn: "arn:aws:sqs:us-east-1:000000000000:dead",
		messageGroupId: "g",
		description: " d ",
	});
	assert.equal(full.ScheduleExpression, "cron(0 12 * * ? *)");
	assert.deepEqual(full.FlexibleTimeWindow, {
		Mode: "FLEXIBLE",
		MaximumWindowInMinutes: 15,
	});
	assert.equal(full.EndDate, 1924992000);
	assert.equal(full.Description, "d");
	assert.deepEqual(full.Target, {
		Arn: queue,
		RoleArn: role,
		Input: '{"a":1}',
		DeadLetterConfig: { Arn: "arn:aws:sqs:us-east-1:000000000000:dead" },
		RetryPolicy: { MaximumEventAgeInSeconds: 120, MaximumRetryAttempts: 3 },
		SqsParameters: { MessageGroupId: "g" },
	});
});

test("schedule input rejects bad values", () => {
	for (const patch of [
		{ name: "" },
		{ name: "a b" },
		{ group: "bad/group" },
		{ timezone: "Mars/Base" },
		{ window: "FLEXIBLE" as const, windowMinutes: "1441" },
		{ start: "2031-01-01T00:00:00Z", end: "2030-01-01T00:00:00Z" },
		{ targetArn: "" },
		{ targetArn: "arn:aws:s3:::bucket" },
		{ roleArn: "" },
		{ input: "{" },
		{ dlqArn: "arn:aws:sns:us-east-1:000000000000:topic" },
		{ retryAge: "59" },
		{ retryAttempts: "186" },
		{ targetArn: "arn:aws:events:us-east-1:000000000000:event-bus/default" },
		{ targetArn: "arn:aws:kinesis:us-east-1:000000000000:stream/s" },
		{ targetArn: "arn:aws:scheduler:::aws-sdk:sqs", input: "{}" },
		{ targetArn: "arn:aws:scheduler:::aws-sdk:sqs:createQueue", input: "" },
	])
		assert.throws(() => scheduleInput({ ...form, ...patch }), Error);
});

test("event bus and Kinesis targets need their parameters", () => {
	const bus = "arn:aws:events:us-east-1:000000000000:event-bus/default";
	assert.deepEqual(
		scheduleInput({ ...form, targetArn: bus, detailType: "t", source: "s" })
			.Target,
		{
			Arn: bus,
			RoleArn: role,
			EventBridgeParameters: { DetailType: "t", Source: "s" },
		},
	);
	const stream = "arn:aws:kinesis:us-east-1:000000000000:stream/s";
	assert.deepEqual(
		scheduleInput({ ...form, targetArn: stream, partitionKey: "k" }).Target,
		{ Arn: stream, RoleArn: role, KinesisParameters: { PartitionKey: "k" } },
	);
});

test("editing round-trips a stored schedule", () => {
	const input = scheduleInput({
		...form,
		kind: "at",
		at: "2030-01-31T09:30:00",
		state: "DISABLED",
		after: "DELETE",
		start: "2030-01-01T00:00:00Z",
		end: "2031-01-01T00:00:00Z",
		window: "FLEXIBLE",
		windowMinutes: "30",
		retryAttempts: "2",
		input: '{"a":1}',
	});
	const stored = {
		...input,
		Arn: "arn",
		CreationDate: 1,
		ScheduleExpressionTimezone: "UTC",
	};
	const again = formFromSchedule(stored as never);
	assert.equal(again.kind, "at");
	assert.equal(again.start, "2030-01-01T00:00:00Z");
	assert.deepEqual(scheduleInput(again), input);
});

test("targets and roles come from resources in the same account and Region", () => {
	const resource = (
		arn: string,
		name: string,
		region: string,
		account = "000000000000",
	) => ({
		service: "x",
		type: "x",
		account,
		region,
		name,
		arn,
	});
	const options = targetOptions(
		[
			resource(queue, "jobs", "us-east-1"),
			resource(
				"arn:aws:sqs:eu-west-1:000000000000:other",
				"other",
				"eu-west-1",
			),
			resource(
				"arn:aws:sqs:us-east-1:111111111111:foreign",
				"foreign",
				"us-east-1",
				"111111111111",
			),
			resource(
				"arn:aws:sns:us-east-1:000000000000:alerts",
				"alerts",
				"us-east-1",
			),
			resource(
				"arn:aws:lambda:us-east-1:000000000000:function:fn",
				"fn",
				"us-east-1",
			),
			resource(
				"arn:aws:lambda:us-east-1:000000000000:layer:l",
				"l",
				"us-east-1",
			),
			resource(role, "scheduler", ""),
			resource("arn:aws:iam::000000000000:user/bob", "bob", ""),
		],
		{ account: "000000000000", region: "us-east-1" },
	);
	assert.deepEqual(
		options.targets.map((o) => o.label),
		["fn", "alerts", "jobs"],
	);
	assert.deepEqual(
		options.queues.map((o) => o.label),
		["jobs"],
	);
	assert.deepEqual(
		options.roles.map((o) => o.label),
		["scheduler"],
	);
	assert.equal(targetService(queue), "sqs");
});
