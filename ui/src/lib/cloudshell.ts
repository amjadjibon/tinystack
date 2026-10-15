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
		JSON.stringify({ command, ...scope }),
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
			JSON.stringify({ command, ...scope, complete: true }),
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
