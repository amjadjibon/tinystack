// The console uses TinyStack's own /_tinystack/ endpoints.
// In development Vite proxies them to the running server.

export type Health = {
	status: string;
	services: string[];
	storage: string;
};

export type Resource = {
	service: string;
	type: string;
	account: string;
	region: string;
	name: string;
	arn: string;
	tags?: Record<string, string>;
	details?: Record<string, unknown>;
};

export type RequestRecord = {
	time: string;
	request_id: string;
	method: string;
	path: string;
	service?: string;
	operation?: string;
	protocol?: string;
	account: string;
	region: string;
	error_code?: string;
	error_message?: string;
	request_body: string;
	response_body: string;
	status: number;
	request_size: number;
	response_size: number;
	duration_ms: number;
};

export type Delivery = {
	time: string;
	via: string;
	source?: string;
	target: string;
	status: string;
	error?: string;
	payload: string;
	size: number;
};

export type MetricSeries = {
	service: string;
	operation: string;
	status: number;
	count: number;
	seconds: number;
};

const base = "/_tinystack";

async function get(path: string): Promise<Response> {
	const response = await fetch(base + path);
	if (!response.ok) {
		throw new Error(`${path}: ${response.status} ${await response.text()}`);
	}

	return response;
}

async function list<T>(path: string, key: string): Promise<T[]> {
	const body = await (await get(path)).json();
	return body[key] ?? [];
}

// parseMetrics turns the Prometheus text format into one row per series.
export function parseMetrics(text: string): MetricSeries[] {
	const series = new Map<string, MetricSeries>();
	const pattern =
		/^(tinystack_\w+)\{service="([^"]*)",operation="([^"]*)",status="(\d+)"\} (\S+)$/;
	for (const line of text.split("\n")) {
		const m = line.match(pattern);
		if (!m) continue;

		const key = `${m[2]} ${m[3]} ${m[4]}`;
		const row = series.get(key) ?? {
			service: m[2],
			operation: m[3],
			status: Number(m[4]),
			count: 0,
			seconds: 0,
		};
		if (m[1] === "tinystack_requests_total") row.count = Number(m[5]);
		if (m[1] === "tinystack_request_duration_seconds_sum")
			row.seconds = Number(m[5]);
		series.set(key, row);
	}

	return [...series.values()];
}

export const api = {
	actionCapabilities: async (): Promise<{
		account: string;
		region: string;
		iam: boolean;
		s3: boolean;
		sqs: boolean;
		sns: boolean;
		lambda: boolean;
		logs: boolean;
		dynamodb: boolean;
		eventbridge: boolean;
		requires_signature: boolean;
	}> => (await get("/actions")).json(),
	action: async <T>(
		service: string,
		operation: string,
		account: string,
		input: Record<string, unknown>,
		region?: string,
	): Promise<T> => {
		const response = await fetch(`${base}/actions`, {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ service, operation, account, region, input }),
		});
		const body = await response.json();
		if (!response.ok) {
			throw new Error(
				`${body.code}: ${body.message}${body.request_id ? ` (Request ID: ${body.request_id})` : ""}`,
			);
		}
		return body.output;
	},
	health: async (): Promise<Health> => (await get("/health")).json(),
	resources: () => list<Resource>("/resources", "resources"),
	requests: () => list<RequestRecord>("/requests", "requests"),
	deliveries: () => list<Delivery>("/deliveries", "deliveries"),
	metrics: async () => parseMetrics(await (await get("/metrics")).text()),
	clear: async (log: "requests" | "deliveries") => {
		const response = await fetch(`${base}/${log}`, { method: "DELETE" });
		if (!response.ok) throw new Error(`${log}: ${response.status}`);
	},
};

// pretty formats JSON for reading and leaves anything else as is.
export function pretty(text: string): string {
	try {
		return JSON.stringify(JSON.parse(text), null, 2);
	} catch {
		return text;
	}
}
