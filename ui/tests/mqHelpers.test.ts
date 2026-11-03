import assert from "node:assert/strict";
import test from "node:test";
import {
	type Broker,
	type BrokerForm,
	brokerInput,
	mqHref,
	pending,
	versions,
} from "../src/lib/mqHelpers.ts";

const f: BrokerForm = {
	name: "test",
	engine: "ACTIVEMQ",
	version: "5.18",
	instance: "mq.m5.large",
	mode: "SINGLE_INSTANCE",
	public: true,
	subnets: "",
	groups: "",
	general: false,
	audit: false,
	day: "MONDAY",
	time: "00:00",
	tags: '{"env":"test"}',
};
const users = [{ Username: "admin", Password: "secure-password-123" }];
test("MQ create includes supported fields and omits public network references", () => {
	const input = brokerInput(f, false, users);
	assert.equal(input.SecurityGroups, undefined);
	assert.equal(input.SubnetIds, undefined);
	assert.deepEqual(input.Tags, { env: "test" });
	assert.deepEqual(input.Users, users);
	assert.deepEqual(input.Logs, { General: false, Audit: false });
});
test("RabbitMQ omits audit and enforces one administrator", () => {
	const rabbit = { ...f, engine: "RABBITMQ", version: "3.13" };
	assert.deepEqual(brokerInput(rabbit, false, users).Logs, { General: false });
	assert.throws(() => brokerInput(rabbit, false, [...users, ...users]));
	assert.throws(() =>
		brokerInput(rabbit, false, [{ ...users[0], Username: "guest" }]),
	);
});
test("MQ validates credentials without echoing them", () => {
	assert.throws(
		() => brokerInput(f, false, [{ Username: "admin", Password: "secret" }]),
		(e) => e instanceof Error && !e.message.includes("secret"),
	);
	assert.throws(() =>
		brokerInput({ ...f, name: "invalid name" }, false, users),
	);
	assert.throws(() => brokerInput({ ...f, version: "4.1" }, false, users));
});
test("updates exclude identity, credentials, and immutable networking", () => {
	const input = brokerInput(
		{ ...f, public: false, groups: "sg-12345678", subnets: "subnet-12345678" },
		false,
		[],
		{ BrokerId: "b-test" } as Broker,
	);
	assert.equal(input.BrokerId, "b-test");
	assert.deepEqual(input.SecurityGroups, ["sg-12345678"]);
	for (const k of [
		"BrokerName",
		"Users",
		"Tags",
		"PubliclyAccessible",
		"SubnetIds",
		"EngineType",
		"DeploymentMode",
	])
		assert.equal(input[k], undefined);
});
test("MQ runtime versions, transitional statuses and scoped navigation", () => {
	assert.deepEqual(versions("RABBITMQ", false), ["3.13"]);
	assert.ok(versions("RABBITMQ", true).includes("4.1"));
	assert.ok(pending("CREATION_IN_PROGRESS"));
	assert.ok(pending("REBOOT_IN_PROGRESS"));
	assert.ok(pending("DELETION_IN_PROGRESS"));
	assert.equal(pending(), false);
	assert.equal(pending("RUNNING"), false);
	const href = mqHref(
		{ account: "000000000000", region: "eu-west-1" },
		"b-test",
		"tags",
	);
	assert.ok(href.includes("broker=b-test"));
	assert.ok(href.includes("tab=tags"));
});

test("private networking and deployment combinations reject incomplete input", () => {
	assert.throws(() => brokerInput({ ...f, public: false }, false, users));
	assert.throws(() =>
		brokerInput(
			{ ...f, instance: "mq.t3.micro", mode: "ACTIVE_STANDBY_MULTI_AZ" },
			false,
			users,
		),
	);
	assert.throws(() => brokerInput({ ...f, time: "24:00" }, false, users));
	const input = brokerInput(
		{ ...f, public: false, subnets: "subnet-12345678", groups: "sg-12345678" },
		false,
		users,
	);
	assert.deepEqual(input.SubnetIds, ["subnet-12345678"]);
	assert.deepEqual(input.SecurityGroups, ["sg-12345678"]);
});
