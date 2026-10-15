import { shellFetch } from "./session.ts";

export type ShellScope = { account: string; region: string };
export type ShellResult = { stdout: string; stderr: string; exit: number };

type ShellBody = Partial<ShellResult> & {
	candidates?: string[];
	code?: string;
	message?: string;
};

async function send(
	command: string,
	scope: ShellScope,
	signal?: AbortSignal,
	complete = false,
) {
	const response = await shellFetch(
		JSON.stringify({
			command,
			account: scope.account,
			region: scope.region,
			...(complete ? { complete: true } : {}),
		}),
		signal,
	);
	const body = (await response.json().catch(() => null)) as ShellBody | null;
	return { response, body };
}

// runCommand sends one command line. A command that fails still returns its
// result, with the message on stderr; only a request TinyStack refuses before
// running anything, or cannot answer, becomes an error result of exit 255.
export async function runCommand(
	command: string,
	scope: ShellScope,
	signal?: AbortSignal,
): Promise<ShellResult> {
	const { response, body } = await send(command, scope, signal);
	if (body && typeof body.exit === "number") {
		return {
			stdout: body.stdout ?? "",
			stderr: body.stderr ?? "",
			exit: body.exit,
		};
	}
	if (body?.code) {
		return { stdout: "", stderr: `${body.code}: ${body.message}\n`, exit: 255 };
	}
	return {
		stdout: "",
		stderr:
			response.status === 404
				? "CloudShell is not available here. Open the console served by a TinyStack built with the console.\n"
				: `The CloudShell request failed (HTTP ${response.status}).\n`,
		exit: 255,
	};
}

// completions lists what could finish the last word of a command line.
export async function completions(line: string, scope: ShellScope) {
	try {
		const { body } = await send(line, scope, undefined, true);
		return body?.candidates ?? [];
	} catch {
		return [];
	}
}

const historyKey = "tinystack.cloudshell.history";

// loadHistory reads the commands typed in this browser tab.
export function loadHistory(): string[] {
	try {
		const saved = JSON.parse(sessionStorage.getItem(historyKey) ?? "[]");
		return Array.isArray(saved)
			? saved.filter((line) => typeof line === "string")
			: [];
	} catch {
		return [];
	}
}

export function saveHistory(history: string[]) {
	try {
		sessionStorage.setItem(historyKey, JSON.stringify(history));
	} catch {
		// The shell works without remembering commands.
	}
}
