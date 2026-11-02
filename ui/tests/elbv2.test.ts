import assert from "node:assert/strict";
import test from "node:test";
import {
	attributeChanges,
	createLoadBalancerInput,
	createTargetGroupInput,
	deleteHint,
	emptyTargetGroupForm,
	hasPendingBalancers,
	modifyTargetGroupInput,
	setSubnetsInput,
	tagsFromRows,
	targetsInput,
	validName,
} from "../src/lib/elbv2Helpers.ts";
import {
	actionFormFrom,
	actionInput,
	conditionForms,
	conditionInput,
	describeAction,
	emptyActionForm,
	listenerInput,
	nextPriority,
	ruleCreateInput,
	rulePrioritiesInput,
} from "../src/lib/elbv2RuleHelpers.ts";

const balancer = {
	name: "web",
	type: "application",
	scheme: "internet-facing",
	ipAddressType: "ipv4",
	subnets: ["subnet-a", "subnet-b"],
	securityGroups: [],
	tags: [{ key: "team", value: "web" }],
};

test("load balancer input checks names, subnets and security groups", () => {
	assert.deepEqual(createLoadBalancerInput(balancer), {
		Name: "web",
		Type: "application",
		Scheme: "internet-facing",
		IpAddressType: "ipv4",
		Subnets: ["subnet-a", "subnet-b"],
		Tags: [{ Key: "team", Value: "web" }],
	});
	assert.throws(() => validName("-bad", "Name"), /hyphen/);
	assert.throws(
		() => createLoadBalancerInput({ ...balancer, subnets: ["subnet-a"] }),
		/two/,
	);
	const network = createLoadBalancerInput({
		...balancer,
		type: "network",
		subnets: ["subnet-a"],
		securityGroups: ["sg-1"],
		tags: [],
	});
	assert.equal("SecurityGroups" in network, false);
	assert.deepEqual(
		createLoadBalancerInput({ ...balancer, securityGroups: ["sg-1"] })
			.SecurityGroups,
		["sg-1"],
	);
	assert.deepEqual(setSubnetsInput("arn", "network", ["s"]), {
		LoadBalancerArn: "arn",
		Subnets: ["s"],
	});
	assert.throws(() => setSubnetsInput("arn", "application", ["s"]), /two/);
});

test("tags and attributes", () => {
	assert.deepEqual(
		tagsFromRows([
			{ key: "a", value: "1" },
			{ key: "", value: "" },
		]),
		{ a: "1" },
	);
	assert.throws(
		() =>
			tagsFromRows([
				{ key: "a", value: "" },
				{ key: "a", value: "" },
			]),
		/repeated/,
	);
	assert.throws(() => tagsFromRows([{ key: "", value: "x" }]), /key/);
	assert.deepEqual(
		attributeChanges(
			[
				{ Key: "idle_timeout.timeout_seconds", Value: "60" },
				{ Key: "deletion_protection.enabled", Value: "false" },
			],
			{
				"idle_timeout.timeout_seconds": "120",
				"deletion_protection.enabled": "false",
			},
		),
		[{ Key: "idle_timeout.timeout_seconds", Value: "120" }],
	);
	assert.equal(hasPendingBalancers([{ State: { Code: "active" } }]), false);
	assert.equal(
		hasPendingBalancers([{ State: { Code: "provisioning" } }]),
		true,
	);
	assert.match(
		deleteHint("deletion protection is enabled"),
		/deletion_protection\.enabled/,
	);
	assert.equal(deleteHint("other"), "other");
});

test("target group input differs for lambda and validates health checks", () => {
	const form = { ...emptyTargetGroupForm, name: "tg", vpc: "vpc-1" };
	const input = createTargetGroupInput(form);
	assert.equal(input.Protocol, "HTTP");
	assert.equal(input.Port, 80);
	assert.deepEqual(input.Matcher, { HttpCode: "200" });
	assert.throws(() => createTargetGroupInput({ ...form, vpc: "" }), /VPC/);
	assert.throws(
		() => createTargetGroupInput({ ...form, timeout: "30" }),
		/smaller/,
	);
	assert.throws(
		() => createTargetGroupInput({ ...form, healthy: "1" }),
		/Healthy/,
	);
	const tcp = createTargetGroupInput({
		...form,
		protocol: "TCP",
		healthProtocol: "TCP",
	});
	assert.equal("HealthCheckPath" in tcp, false);
	const lambda = createTargetGroupInput({
		...form,
		targetType: "lambda",
		vpc: "",
	});
	assert.equal("Protocol" in lambda, false);
	assert.equal("HealthCheckProtocol" in lambda, false);
	assert.equal(
		modifyTargetGroupInput("arn", { ...form, healthPath: "/up" })
			.HealthCheckPath,
		"/up",
	);
});

test("targets carry ports unless lambda", () => {
	assert.deepEqual(targetsInput("arn", "ip", ["10.0.0.1"], "8080").Targets, [
		{ Id: "10.0.0.1", Port: 8080 },
	]);
	assert.deepEqual(targetsInput("arn", "instance", ["i-1"], "").Targets, [
		{ Id: "i-1" },
	]);
	assert.deepEqual(targetsInput("arn", "lambda", ["fn"], "80").Targets, [
		{ Id: "fn" },
	]);
	assert.throws(() => targetsInput("arn", "ip", [], "80"), /at least one/);
});

test("actions build forward, redirect and fixed responses", () => {
	assert.throws(() => actionInput(emptyActionForm), /target group/);
	assert.deepEqual(actionInput({ ...emptyActionForm, targetGroup: "tg-arn" }), [
		{ Type: "forward", Order: 1, TargetGroupArn: "tg-arn" },
	]);
	const redirect = actionInput({
		...emptyActionForm,
		type: "redirect",
		redirectHost: "x.example.com",
	});
	assert.equal(redirect[0].RedirectConfig?.StatusCode, "HTTP_301");
	assert.equal(redirect[0].RedirectConfig?.Host, "x.example.com");
	assert.throws(
		() =>
			actionInput({
				...emptyActionForm,
				type: "fixed-response",
				fixedStatus: "99",
			}),
		/200 to 599/,
	);
	const fixed = actionInput({
		...emptyActionForm,
		type: "fixed-response",
		fixedBody: "down",
	});
	assert.equal(fixed[0].FixedResponseConfig?.MessageBody, "down");
	const form = actionFormFrom(fixed);
	assert.equal(form.type, "fixed-response");
	assert.equal(
		actionFormFrom([{ Type: "forward", TargetGroupArn: "a" }]).targetGroup,
		"a",
	);
	assert.equal(
		describeAction({ Type: "forward", TargetGroupArn: "a" }, () => "name"),
		"Forward to name",
	);
});

test("conditions cover every supported field", () => {
	const conditions = conditionInput([
		{ field: "path-pattern", values: "/api/*\n/v2/*", headerName: "" },
		{ field: "host-header", values: "a.example.com", headerName: "" },
		{ field: "http-header", values: "prod", headerName: "X-Env" },
		{ field: "query-string", values: "v=2", headerName: "" },
	]);
	assert.deepEqual(conditions[0].PathPatternConfig, {
		Values: ["/api/*", "/v2/*"],
	});
	assert.deepEqual(conditions[2].HttpHeaderConfig, {
		HttpHeaderName: "X-Env",
		Values: ["prod"],
	});
	assert.deepEqual(conditions[3].QueryStringConfig, {
		Values: [{ Key: "v", Value: "2" }],
	});
	assert.deepEqual(
		conditionInput([
			{ field: "source-ip", values: "10.0.0.0/8", headerName: "" },
		])[0].SourceIpConfig,
		{ Values: ["10.0.0.0/8"] },
	);
	assert.deepEqual(
		conditionInput([
			{ field: "http-request-method", values: "get", headerName: "" },
		])[0].HttpRequestMethodConfig,
		{ Values: ["GET"] },
	);
	assert.throws(() => conditionInput([]), /at least one/);
	assert.throws(
		() =>
			conditionInput([{ field: "http-header", values: "x", headerName: "" }]),
		/header name/,
	);
	assert.throws(
		() =>
			conditionInput([
				{ field: "path-pattern", values: "/a", headerName: "" },
				{ field: "path-pattern", values: "/b", headerName: "" },
			]),
		/only one/,
	);
	assert.throws(
		() =>
			conditionInput([
				{ field: "path-pattern", values: "1\n2\n3\n4\n5\n6", headerName: "" },
			]),
		/at most 5/,
	);
	assert.deepEqual(conditionForms(conditions)[3], {
		field: "query-string",
		values: "v=2",
		headerName: "",
	});
});

test("listener and rule inputs", () => {
	const form = {
		protocol: "HTTP",
		port: "80",
		certificate: "",
		sslPolicy: "",
		action: { ...emptyActionForm, targetGroup: "tg" },
	};
	assert.equal("Certificates" in listenerInput(form), false);
	assert.throws(() => listenerInput({ ...form, port: "0" }), /Port/);
	assert.throws(
		() => listenerInput({ ...form, protocol: "HTTPS" }),
		/certificate/,
	);
	assert.throws(
		() => listenerInput({ ...form, protocol: "HTTPS", certificate: "arn" }),
		/SSL policy/,
	);
	const secure = listenerInput({
		...form,
		protocol: "HTTPS",
		certificate: "arn",
		sslPolicy: "p",
	});
	assert.deepEqual(secure.Certificates, [{ CertificateArn: "arn" }]);
	assert.equal(secure.SslPolicy, "p");
	const rule = ruleCreateInput(
		"listener",
		"10",
		[{ field: "path-pattern", values: "/a", headerName: "" }],
		form.action,
	);
	assert.equal(rule.Priority, 10);
	assert.throws(
		() =>
			ruleCreateInput(
				"l",
				"50001",
				[{ field: "path-pattern", values: "/a", headerName: "" }],
				form.action,
			),
		/Priority/,
	);
});

test("rule priorities detect duplicates and changes", () => {
	const rules = [
		{ RuleArn: "a", Priority: "10" },
		{ RuleArn: "b", Priority: "20" },
		{ RuleArn: "d", Priority: "default", IsDefault: true },
	];
	assert.deepEqual(rulePrioritiesInput(rules, { a: "30" }).RulePriorities, [
		{ RuleArn: "a", Priority: 30 },
	]);
	assert.throws(() => rulePrioritiesInput(rules, { a: "20" }), /more than one/);
	assert.throws(() => rulePrioritiesInput(rules, {}), /No rule priority/);
	assert.throws(() => rulePrioritiesInput(rules, { a: "x" }), /whole number/);
	assert.equal(nextPriority(rules), 30);
});
