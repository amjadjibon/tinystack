import assert from "node:assert/strict";
import { test } from "node:test";
import { builtin } from "../src/lib/cloudshell.ts";
import {
	columns,
	commonPrefix,
	LineEditor,
	parseKeys,
} from "../src/lib/lineEditor.ts";

test("keys are split from typed and pasted data", () => {
	assert.deepEqual(parseKeys("abc"), [{ type: "text", text: "abc" }]);
	assert.deepEqual(parseKeys("a\x1b[Db\x7f\r"), [
		{ type: "text", text: "a" },
		{ type: "left" },
		{ type: "text", text: "b" },
		{ type: "backspace" },
		{ type: "enter" },
	]);
	assert.deepEqual(parseKeys("\x1b[A\x1b[B\x1b[3~\x1b[H\x1b[F"), [
		{ type: "up" },
		{ type: "down" },
		{ type: "delete" },
		{ type: "home" },
		{ type: "end" },
	]);
	assert.deepEqual(parseKeys("\x03\x0c\t\x01\x05\x15\x0b\x17"), [
		{ type: "interrupt" },
		{ type: "clear" },
		{ type: "tab" },
		{ type: "home" },
		{ type: "end" },
		{ type: "killToStart" },
		{ type: "killToEnd" },
		{ type: "killWord" },
	]);
	// A paste of two lines is two commands; unknown sequences vanish.
	assert.deepEqual(parseKeys("one\ntwo\x1b[99;5~x"), [
		{ type: "text", text: "one" },
		{ type: "enter" },
		{ type: "text", text: "two" },
		{ type: "text", text: "x" },
	]);
});

function type(editor: LineEditor, data: string) {
	for (const key of parseKeys(data)) editor.apply(key);
}

test("the editor inserts, deletes and moves the cursor", () => {
	const editor = new LineEditor();
	type(editor, "aws sqs lst");
	type(editor, "\x1b[D\x1b[D"); // two left
	type(editor, "i");
	assert.equal(editor.buffer, "aws sqs list");
	assert.equal(editor.cursor, 10);
	type(editor, "\x01");
	assert.equal(editor.cursor, 0);
	type(editor, "\x1b[3~");
	assert.equal(editor.buffer, "ws sqs list");
	type(editor, "\x05\x17");
	assert.equal(editor.buffer, "ws sqs ");
	type(editor, "\x15");
	assert.equal(editor.buffer, "");
	type(editor, "abc\x1b[D\x0b");
	assert.equal(editor.buffer, "ab");
	type(editor, "\x7f\x7f\x7f");
	assert.equal(editor.buffer, "");
	assert.equal(editor.cursor, 0);
});

test("history walks back and forth and keeps the unfinished line", () => {
	const editor = new LineEditor(["first", "second"]);
	type(editor, "draft");
	type(editor, "\x1b[A");
	assert.equal(editor.buffer, "second");
	type(editor, "\x1b[A\x1b[A");
	assert.equal(editor.buffer, "first");
	type(editor, "\x1b[B\x1b[B");
	assert.equal(editor.buffer, "draft");
	assert.equal(editor.submit(), "draft");
	assert.deepEqual(editor.history, ["first", "second", "draft"]);
	assert.equal(editor.buffer, "");

	type(editor, "draft");
	editor.submit();
	type(editor, "  ");
	editor.submit();
	assert.deepEqual(editor.history, ["first", "second", "draft"]);

	const small = new LineEditor();
	for (const line of ["a", "b", "c"]) {
		type(small, line);
		small.submit(2);
	}
	assert.deepEqual(small.history, ["b", "c"]);
});

test("the word before the cursor is replaced by a completion", () => {
	const editor = new LineEditor();
	type(editor, "aws sq");
	assert.equal(editor.word(), "sq");
	editor.replaceWord("sqs ");
	assert.equal(editor.buffer, "aws sqs ");
	assert.equal(editor.cursor, 8);
	assert.equal(editor.word(), "");
});

test("completions share a prefix and fit the terminal", () => {
	assert.equal(commonPrefix(["create-queue", "create-topic"]), "create-");
	assert.equal(commonPrefix(["only"]), "only");
	assert.equal(commonPrefix(["a", "b"]), "");
	assert.equal(commonPrefix([]), "");
	assert.deepEqual(columns(["aa", "bb", "cc", "dd"], 8), ["aa  bb", "cc  dd"]);
});

test("commands stream output and return clear request failures", async () => {
	const { runCommand, completions, saveHistory, loadHistory } = await import(
		"../src/lib/cloudshell.ts"
	);
	const originalFetch = globalThis.fetch;
	const scope = { account: "000000000000", region: "us-east-1" };
	const sent: { url: string; body: Record<string, unknown> }[] = [];
	let reply: () => Response = () => new Response("{}");
	globalThis.fetch = (async (url: URL | string, init?: RequestInit) => {
		sent.push({ url: String(url), body: JSON.parse(String(init?.body)) });
		return reply();
	}) as typeof fetch;
	try {
		const chunks: unknown[] = [];
		reply = () =>
			new Response(
				new ReadableStream({
					start(controller) {
						const encoder = new TextEncoder();
						controller.enqueue(
							encoder.encode('{"stream":"stdout","data":"first"}\n{"str'),
						);
						controller.enqueue(
							encoder.encode('eam":"stderr","data":"second"}\n{"exit":7}\n'),
						);
						controller.close();
					},
				}),
			);
		assert.deepEqual(
			await runCommand("aws s3 ls", scope, undefined, (c) => chunks.push(c)),
			{ exit: 7 },
		);
		assert.deepEqual(chunks, [
			{ stream: "stdout", data: "first" },
			{ stream: "stderr", data: "second" },
		]);
		assert.equal(sent[0].url, "/_tinystack/shell");
		assert.deepEqual(sent[0].body, { command: "aws s3 ls", ...scope });
		reply = () =>
			Response.json(
				{ code: "AWSCLINotInstalled", message: "install CLI" },
				{ status: 503 },
			);
		assert.deepEqual(
			await runCommand("aws s3 ls", scope, undefined, (c) => chunks.push(c)),
			{ exit: 255, banner: "install CLI" },
		);
		reply = () => Response.json({ candidates: ["s3"] });
		assert.deepEqual(await completions("aws s", scope), ["s3"]);
		assert.equal(sent.at(-1)?.body.complete, true);
		saveHistory(["secret"]);
		const history = loadHistory();
		history.push("changed");
		assert.deepEqual(loadHistory(), ["secret"]);
	} finally {
		globalThis.fetch = originalFetch;
	}
});

test("built-ins run in the terminal and other words are not found", () => {
	assert.equal(builtin("aws s3 ls", []), undefined);
	assert.equal(builtin("aws-local s3 ls", []), undefined);
	assert.equal(builtin("awslocal s3 ls", []), undefined);
	assert.deepEqual(builtin(" clear ", []), { type: "clear" });
	assert.deepEqual(builtin("reset", []), { type: "clear" });
	assert.deepEqual(builtin("history -c", ["a"]), { type: "clearHistory" });
	assert.deepEqual(builtin("history", ["aws s3 ls", "whoami"]), {
		type: "print",
		text: "  1  aws s3 ls\n  2  whoami",
	});
	assert.deepEqual(builtin("echo  hello   world", []), {
		type: "print",
		text: "hello world",
	});
	assert.deepEqual(builtin("whoami", []), {
		type: "run",
		command: "aws sts get-caller-identity --query Arn --output text",
	});
	const missing = builtin("pwd", []);
	assert.equal(missing?.type, "print");
	assert.match(
		missing?.type === "print" ? missing.text : "",
		/^pwd: command not found/,
	);
});

test("history -c forgets the typed commands", () => {
	const editor = new LineEditor(["one", "two"]);
	editor.forget();
	assert.deepEqual(editor.history, []);
	editor.apply({ type: "up" });
	assert.equal(editor.buffer, "");
});
