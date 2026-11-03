import { tagList } from "./tags.ts";
// Pure logic for the Elastic Load Balancing page: payload builders and checks.

export const applicationListenerProtocols = ["HTTP", "HTTPS"];
export const networkListenerProtocols = ["TCP", "TLS", "UDP", "TCP_UDP"];
export const applicationTargetProtocols = ["HTTP", "HTTPS"];
export const networkTargetProtocols = ["TCP", "TLS", "UDP", "TCP_UDP"];

export const listenerProtocols = (type?: string) =>
	type === "network" ? networkListenerProtocols : applicationListenerProtocols;
export const isSecureProtocol = (protocol: string) =>
	protocol === "HTTPS" || protocol === "TLS";

const namePattern = /^(?!-)[A-Za-z0-9-]{1,32}(?<!-)$/;

export function validName(name: string, label: string) {
	const value = name.trim();
	if (!namePattern.test(value))
		throw new Error(
			`${label} must be 1 to 32 letters, digits or hyphens and cannot begin or end with a hyphen.`,
		);
	return value;
}

export function parseInteger(
	text: string,
	label: string,
	min: number,
	max: number,
) {
	const value = text.trim();
	const n = Number(value);
	if (!/^\d+$/.test(value) || n < min || n > max)
		throw new Error(`${label} must be a whole number from ${min} to ${max}.`);
	return n;
}

// lines splits a textarea into trimmed non-empty entries.
export function lines(text: string) {
	return text
		.split(/[\n,]/)
		.map((v) => v.trim())
		.filter(Boolean);
}

export const isPendingState = (code?: string) => code === "provisioning";
export const hasPendingBalancers = (items: { State?: { Code?: string } }[]) =>
	items.some((i) => isPendingState(i.State?.Code));

// tagsFromRows rejects blank and repeated keys.
export function tagsFromRows(rows: { key: string; value: string }[]) {
	const out: Record<string, string> = {};
	for (const { key, value } of rows) {
		const k = key.trim();
		if (!k) {
			if (value.trim()) throw new Error("Every tag needs a key.");
			continue;
		}
		if (Object.hasOwn(out, k)) throw new Error(`Tag key ${k} is repeated.`);
		out[k] = value;
	}
	return out;
}

// attributeChanges lists only the attributes whose value was edited.
export function attributeChanges(
	original: { Key: string; Value?: string }[],
	edited: Record<string, string>,
) {
	return original
		.filter(
			(a) => Object.hasOwn(edited, a.Key) && edited[a.Key] !== (a.Value ?? ""),
		)
		.map((a) => ({ Key: a.Key, Value: edited[a.Key] }));
}

export const knownLoadBalancerKeys = [
	"idle_timeout.timeout_seconds",
	"deletion_protection.enabled",
	"load_balancing.cross_zone.enabled",
	"routing.http2.enabled",
	"access_logs.s3.enabled",
];

export type LoadBalancerForm = {
	name: string;
	type: string;
	scheme: string;
	ipAddressType: string;
	subnets: string[];
	securityGroups: string[];
	tags: { key: string; value: string }[];
};

export function createLoadBalancerInput(f: LoadBalancerForm) {
	const name = validName(f.name, "Load balancer name");
	const application = f.type === "application";
	if (!f.subnets.length) throw new Error("Choose at least one subnet.");
	if (application && f.subnets.length < 2)
		throw new Error(
			"An application load balancer needs subnets in at least two Availability Zones.",
		);
	const tags = tagsFromRows(f.tags);
	return {
		Name: name,
		Type: f.type,
		Scheme: f.scheme,
		IpAddressType: f.ipAddressType,
		Subnets: f.subnets,
		...(application && f.securityGroups.length
			? { SecurityGroups: f.securityGroups }
			: {}),
		...(Object.keys(tags).length ? { Tags: tagList(tags) } : {}),
	};
}

export function setSubnetsInput(
	arn: string,
	type: string | undefined,
	subnets: string[],
) {
	if (!subnets.length) throw new Error("Choose at least one subnet.");
	if (type !== "network" && subnets.length < 2)
		throw new Error(
			"An application load balancer needs subnets in at least two Availability Zones.",
		);
	return { LoadBalancerArn: arn, Subnets: subnets };
}

export type TargetGroupForm = {
	name: string;
	targetType: string;
	protocol: string;
	port: string;
	vpc: string;
	healthProtocol: string;
	healthPort: string;
	healthPath: string;
	interval: string;
	timeout: string;
	healthy: string;
	unhealthy: string;
	matcher: string;
	tags: { key: string; value: string }[];
};

export const emptyTargetGroupForm: TargetGroupForm = {
	name: "",
	targetType: "instance",
	protocol: "HTTP",
	port: "80",
	vpc: "",
	healthProtocol: "HTTP",
	healthPort: "traffic-port",
	healthPath: "/",
	interval: "30",
	timeout: "5",
	healthy: "5",
	unhealthy: "2",
	matcher: "200",
	tags: [],
};

export const isWebProtocol = (p: string) => p === "HTTP" || p === "HTTPS";

function healthInput(f: TargetGroupForm, lambda: boolean) {
	const interval = parseInteger(f.interval, "Health check interval", 5, 300);
	const timeout = parseInteger(f.timeout, "Health check timeout", 2, 120);
	if (timeout >= interval)
		throw new Error(
			"The health check timeout must be smaller than the interval.",
		);
	const web = lambda || isWebProtocol(f.healthProtocol);
	const port = f.healthPort.trim() || "traffic-port";
	if (!lambda && port !== "traffic-port")
		parseInteger(port, "Health check port", 1, 65535);
	return {
		...(lambda
			? {}
			: { HealthCheckProtocol: f.healthProtocol, HealthCheckPort: port }),
		HealthCheckIntervalSeconds: interval,
		HealthCheckTimeoutSeconds: timeout,
		HealthyThresholdCount: parseInteger(f.healthy, "Healthy threshold", 2, 10),
		UnhealthyThresholdCount: parseInteger(
			f.unhealthy,
			"Unhealthy threshold",
			2,
			10,
		),
		...(web
			? {
					HealthCheckPath: f.healthPath.trim() || "/",
					Matcher: { HttpCode: f.matcher.trim() || "200" },
				}
			: {}),
	};
}

export function createTargetGroupInput(f: TargetGroupForm) {
	const lambda = f.targetType === "lambda";
	const tags = tagsFromRows(f.tags);
	if (!lambda && !f.vpc) throw new Error("Choose a VPC.");
	return {
		Name: validName(f.name, "Target group name"),
		TargetType: f.targetType,
		...(lambda
			? {}
			: {
					Protocol: f.protocol,
					Port: parseInteger(f.port, "Port", 1, 65535),
					VpcId: f.vpc,
				}),
		...healthInput(f, lambda),
		...(Object.keys(tags).length ? { Tags: tagList(tags) } : {}),
	};
}

export function modifyTargetGroupInput(arn: string, f: TargetGroupForm) {
	const input = healthInput(f, f.targetType === "lambda");
	return { TargetGroupArn: arn, ...input };
}

export function targetGroupFormFrom(g: {
	TargetGroupName: string;
	TargetType?: string;
	Protocol?: string;
	Port?: number;
	VpcId?: string;
	HealthCheckProtocol?: string;
	HealthCheckPort?: string;
	HealthCheckPath?: string;
	HealthCheckIntervalSeconds?: number;
	HealthCheckTimeoutSeconds?: number;
	HealthyThresholdCount?: number;
	UnhealthyThresholdCount?: number;
	Matcher?: { HttpCode?: string };
}): TargetGroupForm {
	return {
		name: g.TargetGroupName,
		targetType: g.TargetType ?? "instance",
		protocol: g.Protocol ?? "",
		port: String(g.Port ?? ""),
		vpc: g.VpcId ?? "",
		healthProtocol: g.HealthCheckProtocol ?? g.Protocol ?? "HTTP",
		healthPort: g.HealthCheckPort ?? "traffic-port",
		healthPath: g.HealthCheckPath ?? "/",
		interval: String(g.HealthCheckIntervalSeconds ?? 30),
		timeout: String(g.HealthCheckTimeoutSeconds ?? 5),
		healthy: String(g.HealthyThresholdCount ?? 5),
		unhealthy: String(g.UnhealthyThresholdCount ?? 2),
		matcher: g.Matcher?.HttpCode ?? "200",
		tags: [],
	};
}

// targetsInput builds Targets for Register or DeregisterTargets. Lambda
// targets carry a function ARN and no port; others need a port unless the
// group's port applies.
export function targetsInput(
	arn: string,
	targetType: string | undefined,
	ids: string[],
	port: string,
) {
	if (!ids.length) throw new Error("Choose or enter at least one target.");
	const portless = targetType === "lambda" || !port.trim();
	const n = portless ? undefined : parseInteger(port, "Port", 1, 65535);
	return {
		TargetGroupArn: arn,
		Targets: ids.map((Id) => ({ Id, ...(n === undefined ? {} : { Port: n }) })),
	};
}

export function deleteHint(message: string) {
	return /deletion protection/i.test(message)
		? `${message} Turn off deletion_protection.enabled in the load balancer attributes, then delete it again.`
		: message;
}
