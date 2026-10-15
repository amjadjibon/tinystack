import { shellFetch } from "./session.ts";

export type ShellScope = { account: string; region: string };
export type ShellChunk = { stream: "stdout" | "stderr"; data: string };
export type ShellResult = { exit: number; banner?: string };

type ShellBody = {
	candidates?: string[];
	code?: string;
	message?: string;
};

export async function runCommand(
	command: string,
	scope: ShellScope,
	signal: AbortSignal | undefined,
	onChunk: (chunk: ShellChunk) => void,
): Promise<ShellResult> {
	const response = await shellFetch(
		JSON.stringify({ command, account: scope.account, region: scope.region }),
		signal,
	);
	if (!response.ok) {
		const body = (await response.json().catch(() => null)) as ShellBody | null;
		const message =
			body?.message ??
			`The CloudShell request failed (HTTP ${response.status}).`;
		onChunk({ stream: "stderr", data: `${message}\n` });
		return {
			exit: 255,
			...(body?.code === "AWSCLINotInstalled" ? { banner: message } : {}),
		};
	}
	if (!response.body) throw new Error("The CloudShell response has no stream.");
	const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
	let pending = "";
	let exit: number | undefined;
	try {
		for (;;) {
			const { value, done } = await reader.read();
			if (done) break;
			pending += value;
			let end = pending.indexOf("\n");
			while (end >= 0) {
				const chunk = JSON.parse(
					pending.slice(0, end),
				) as Partial<ShellChunk> & { exit?: number };
				pending = pending.slice(end + 1);
				if (typeof chunk.exit === "number") exit = chunk.exit;
				else if (
					(chunk.stream === "stdout" || chunk.stream === "stderr") &&
					typeof chunk.data === "string"
				)
					onChunk({ stream: chunk.stream, data: chunk.data });
				end = pending.indexOf("\n");
			}
		}
	} finally {
		reader.releaseLock();
	}
	if (exit === undefined)
		throw new Error(
			"The CloudShell stream ended before the command completed.",
		);
	return { exit };
}

export async function completions(command: string, scope: ShellScope) {
	try {
		const response = await shellFetch(
			JSON.stringify({
				command,
				account: scope.account,
				region: scope.region,
				complete: true,
			}),
		);
		const body = (await response.json()) as ShellBody;
		return body.candidates ?? [];
	} catch {
		return [];
	}
}

let history: string[] = [];
export function loadHistory(): string[] {
	return [...history];
}
export function saveHistory(lines: string[]) {
	history = [...lines];
}

// What a built-in asks the terminal to do. Lines that start with aws go to
// the server instead.
export type Builtin =
	| { type: "clear" }
	| { type: "clearHistory" }
	| { type: "print"; text: string }
	| { type: "run"; command: string };

const help = [
	"Run aws commands with the host AWS CLI, as your console identity.",
	"Tab completes; Ctrl-C cancels; Up/Down walk the history; Ctrl-L clears.",
	"",
	"Built-ins:",
	"  clear, reset     clear the screen",
	"  history [-c]     list (or forget) the commands typed in this tab",
	"  echo [text]      print text",
	"  whoami           print the ARN the AWS CLI runs as",
	"  help             print this help",
	"",
	"There is no shell: pipes, redirection, variables, && and files are unavailable.",
].join("\n");

// builtin interprets a line the terminal handles itself, or returns undefined
// for an aws command.
export function builtin(line: string, history: string[]): Builtin | undefined {
	const [name = "", ...args] = line.trim().split(/\s+/);
	switch (name) {
		case "aws":
			return undefined;
		case "clear":
		case "reset":
			return { type: "clear" };
		case "help":
			return { type: "print", text: help };
		case "history": {
			if (args[0] === "-c") return { type: "clearHistory" };
			const width = String(history.length).length + 2;
			return {
				type: "print",
				text: history
					.map((h, i) => `${String(i + 1).padStart(width)}  ${h}`)
					.join("\n"),
			};
		}
		case "echo":
			return { type: "print", text: args.join(" ") };
		case "whoami":
			return {
				type: "run",
				command: "aws sts get-caller-identity --query Arn --output text",
			};
		case "exit":
		case "logout":
			return {
				type: "print",
				text: "CloudShell has no session to exit; close the tab or leave the page.",
			};
		default:
			return {
				type: "print",
				text: `${name}: command not found. CloudShell runs aws commands; type help for the built-ins.`,
			};
	}
}
