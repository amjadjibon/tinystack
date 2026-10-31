import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";
import { service } from "../src/lib/services.ts";
import {
	arn,
	type Check,
	type Context,
	check,
	emptySnapshot,
	type Snapshot,
} from "../src/tutorials/checks.ts";
import { HOTSPOT } from "../src/tutorials/hotspots.ts";
import { tutorials } from "../src/tutorials/index.ts";
import {
	clearProgress,
	fresh,
	load,
	restart,
	type Store,
	save,
} from "../src/tutorials/progress.ts";

test("definitions and rendered hotspot registry are complete", () => {
	assert.equal(new Set(tutorials.map((t) => t.id)).size, tutorials.length);
	const ids = new Set<string>();
	for (const t of tutorials) {
		assert.match(t.id, /^[a-z0-9]+(?:-[a-z0-9]+)+$/);
		assert.ok(t.tasks.length);
		for (const s of t.services) assert.notEqual(service(s).title, s);
		for (const task of t.tasks) {
			assert.ok(task.steps.length);
			for (const s of task.steps) {
				assert.ok(
					s.title && s.content && s.route.startsWith("/services/") && s.command,
				);
				assert.ok(Object.values(HOTSPOT).includes(s.hotspotId));
				ids.add(s.hotspotId);
			}
		}
	}
	const rendered = new Set<string>();
	function scan(dir: string) {
		for (const entry of readdirSync(dir, { withFileTypes: true })) {
			const path = join(dir, entry.name);
			if (entry.isDirectory()) scan(path);
			else if (path.endsWith(".tsx")) {
				const text = readFileSync(path, "utf8");
				for (const m of text.matchAll(/hotspotId=\{HOTSPOT\.(\w+)\}/g)) {
					assert.ok(m[1] in HOTSPOT);
					rendered.add(HOTSPOT[m[1] as keyof typeof HOTSPOT]);
				}
				assert.doesNotMatch(text, /hotspotId=["']/);
			}
		}
	}
	scan(new URL("../src", import.meta.url).pathname);
	assert.deepEqual([...ids].sort(), Object.values(HOTSPOT).sort());
	assert.deepEqual([...rendered].sort(), [...ids].sort());
});
const ctx: Context = {
	account: "000000000000",
	region: "us-east-1",
	names: {
		bucket: "tutorial-s3-1234",
		key: "hello.txt",
		topic: "tutorial-sns-1234",
		queue: "tutorial-sqs-1234",
	},
};
function fixture(kind: Check): Snapshot {
	const s = emptySnapshot();
	const c = ctx;
	const n = c.names;
	if (["bucket", "topic", "queue", "subscription"].includes(kind)) {
		const name =
			kind === "bucket" ? n.bucket : kind === "queue" ? n.queue : n.topic;
		const svc = kind === "bucket" ? "s3" : kind === "queue" ? "sqs" : "sns";
		s.resources.push({
			service: svc,
			type: kind,
			account: c.account,
			region: c.region,
			name,
			arn: arn(svc, name, c),
			details: {
				Subscriptions: [
					{
						SubscriptionArn: `${arn("sns", n.topic, c)}:id`,
						Protocol: "sqs",
						Endpoint: arn("sqs", n.queue, c),
					},
				],
			},
		});
	} else if (kind === "delivery")
		s.deliveries.push({
			time: "",
			via: "sns",
			source: arn("sns", n.topic, c),
			target: arn("sqs", n.queue, c),
			status: "delivered",
			payload: "",
			size: 0,
		});
	else
		s.requests.push({
			time: "",
			request_id: "",
			method: "POST",
			path: "/_tinystack/actions",
			account: c.account,
			region: c.region,
			service: kind === "receive" ? "sqs" : "s3",
			operation:
				kind === "object"
					? "PutObject"
					: kind === "download"
						? "GetObject"
						: "ReceiveMessage",
			status: 200,
			request_body: JSON.stringify({
				input:
					kind === "receive"
						? { QueueUrl: `/${c.account}/${n.queue}` }
						: { Bucket: n.bucket, Key: n.key },
			}),
			response_body: JSON.stringify({
				output: { Messages: [{ Body: "Hello" }] },
			}),
			request_size: 0,
			response_size: 0,
			duration_ms: 0,
		});
	return s;
}
for (const kind of [
	"bucket",
	"object",
	"download",
	"topic",
	"queue",
	"subscription",
	"delivery",
	"receive",
] as const) {
	test(`${kind}: pass, absent, other account, Region and names`, () => {
		const s = fixture(kind);
		assert.equal(check(kind, s, ctx), true);
		assert.equal(check(kind, emptySnapshot(), ctx), false);
		assert.equal(check(kind, s, { ...ctx, account: "111111111111" }), false);
		assert.equal(check(kind, s, { ...ctx, region: "eu-west-1" }), false);
		assert.equal(
			check(kind, s, {
				...ctx,
				names: {
					bucket: "other",
					key: "other",
					topic: "other",
					queue: "other",
				},
			}),
			false,
		);
		if (s.requests.length) {
			s.requests[0].status = 403;
			assert.equal(check(kind, s, ctx), false);
		}
		if (s.deliveries.length) {
			s.deliveries[0].status = "failed";
			assert.equal(check(kind, s, ctx), false);
		}
	});
}
test("wire S3 requests match exact key and failed or empty receives do not pass", () => {
	const s = fixture("download");
	s.requests[0].request_body = "";
	s.requests[0].path = `/${ctx.names.bucket}/${ctx.names.key}`;
	assert.ok(check("download", s, ctx));
	s.requests[0].path += "-other";
	assert.equal(check("download", s, ctx), false);
	const r = fixture("receive");
	r.requests[0].response_body = '{"output":{"Messages":[]}}';
	assert.equal(check("receive", r, ctx), false);
});
test("progress survives reload, isolates scopes, restarts and resets only tutorial keys", () => {
	const data = new Map<string, string>();
	const store: Store = {
		getItem: (k) => data.get(k) ?? null,
		setItem: (k, v) => {
			data.set(k, v);
		},
		removeItem: (k) => {
			data.delete(k);
		},
		key: (i) => [...data.keys()][i] ?? null,
		get length() {
			return data.size;
		},
	};
	const p = { ...fresh("s3-first-bucket"), step: 2, completed: [0, 1] };
	save(store, "s3-first-bucket", ctx.account, ctx.region, p);
	assert.deepEqual(load(store, "s3-first-bucket", ctx.account, ctx.region), p);
	assert.equal(load(store, "sns-sqs-fanout", ctx.account, ctx.region), null);
	assert.equal(load(store, "s3-first-bucket", "other", ctx.region), null);
	assert.equal(load(store, "s3-first-bucket", ctx.account, "other"), null);
	restart(store, "s3-first-bucket", ctx.account, ctx.region);
	assert.equal(load(store, "s3-first-bucket", ctx.account, ctx.region), null);
	save(store, "s3-first-bucket", ctx.account, ctx.region, p);
	save(store, "sns-sqs-fanout", ctx.account, "eu-west-1", p);
	store.setItem("unrelated", "keep");
	clearProgress(store);
	assert.equal(store.length, 1);
	assert.equal(store.getItem("unrelated"), "keep");
});

test("copied step commands work in CloudShell without a host shell", () => {
	for (const tutorial of tutorials)
		for (const task of tutorial.tasks)
			for (const step of task.steps) {
				assert.match(step.command, /^aws-local /);
				assert.doesNotMatch(
					step.command,
					/\n|\$\(|\$[A-Z]|\bexport\b|\bprintf\b|[<>|]/,
				);
			}
});
