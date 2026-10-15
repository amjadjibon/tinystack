import { FitAddon } from "@xterm/addon-fit";
import { type ITheme, Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { useEffect, useRef, useState } from "react";
import {
	builtin,
	completions,
	loadHistory,
	runCommand,
	type ShellScope,
	saveHistory,
} from "#/lib/cloudshell";
import {
	columns,
	commonPrefix,
	type Key,
	LineEditor,
	parseKeys,
} from "#/lib/lineEditor";

const prompt = "\x1b[32m$\x1b[0m ";
const red = (text: string) => `\x1b[31m${text}\x1b[0m`;
const dim = (text: string) => `\x1b[2m${text}\x1b[0m`;

const light: ITheme = {
	background: "#ffffff",
	foreground: "#0f141a",
	cursor: "#0f141a",
	selectionBackground: "#b8d9ff",
	green: "#037f0c",
	red: "#d91515",
};
const dark: ITheme = {
	background: "#161e27",
	foreground: "#e9ebed",
	cursor: "#e9ebed",
	selectionBackground: "#2e4a6d",
	green: "#2bb34a",
	red: "#ff7a7a",
};

// The console marks <body> with this class in dark mode (ui/src/lib/theme.ts).
const isDark = () => document.body.classList.contains("awsui-dark-mode");

const lines = (text: string) => text.replace(/\r?\n/g, "\r\n");

// CloudShellTerminal is an xterm.js terminal that sends each command line to
// TinyStack. It is loaded on demand, so the console's first page does not
// carry the terminal.
export default function CloudShellTerminal({ scope }: { scope: ShellScope }) {
	const [banner, setBanner] = useState<string>();
	const container = useRef<HTMLDivElement>(null);
	const current = useRef(scope);
	current.current = scope;

	useEffect(() => {
		const host = container.current;
		if (!host) return;
		const term = new Terminal({
			cursorBlink: true,
			fontSize: 13,
			fontFamily: "Menlo, Monaco, Consolas, 'Courier New', monospace",
			scrollback: 5000,
			theme: isDark() ? dark : light,
		});
		const fit = new FitAddon();
		term.loadAddon(fit);
		term.open(host);
		fit.fit();
		term.focus();

		const editor = new LineEditor(loadHistory());
		let running: AbortController | undefined;
		let stopped = false;

		const redraw = () => {
			const back = editor.buffer.length - editor.cursor;
			term.write(
				`\r\x1b[2K${prompt}${editor.buffer}${back > 0 ? `\x1b[${back}D` : ""}`,
			);
		};
		const newPrompt = () => {
			if (!stopped) redraw();
		};

		const execute = async (line: string) => {
			const command = line.trim();
			saveHistory(editor.history);
			if (!command) return newPrompt();
			const action = builtin(command, editor.history);
			if (action?.type === "clear") {
				term.write("\x1b[2J\x1b[3J\x1b[H");
				return newPrompt();
			}
			if (action?.type === "clearHistory") {
				editor.forget();
				saveHistory(editor.history);
				return newPrompt();
			}
			if (action?.type === "print") {
				if (action.text) term.write(`${lines(action.text)}\r\n`);
				return newPrompt();
			}
			running = new AbortController();
			try {
				const result = await runCommand(
					action?.command ?? command,
					current.current,
					running.signal,
					({ stream, data }) =>
						term.write(stream === "stderr" ? red(lines(data)) : lines(data)),
				);
				setBanner(result.banner);
			} catch (error) {
				if (!(error instanceof DOMException && error.name === "AbortError"))
					term.write(
						red(`${error instanceof Error ? error.message : error}\r\n`),
					);
			} finally {
				running = undefined;
			}
			newPrompt();
		};

		const complete = async () => {
			const before = editor.buffer.slice(0, editor.cursor);
			const candidates = await completions(before, current.current);
			if (candidates.length === 0) return;
			const word = editor.word();
			const prefix = commonPrefix(candidates);
			if (candidates.length === 1) {
				editor.replaceWord(`${prefix} `);
			} else if (prefix.length > word.length) {
				editor.replaceWord(prefix);
			} else {
				term.write(`\r\n${columns(candidates, term.cols).join("\r\n")}\r\n`);
			}
			redraw();
		};

		const handle = async (key: Key) => {
			switch (key.type) {
				case "enter":
					term.write("\r\n");
					await execute(editor.submit());
					return;
				case "interrupt":
					term.write("^C\r\n");
					editor.clearLine();
					newPrompt();
					return;
				case "clear":
					term.write("\x1b[2J\x1b[3J\x1b[H");
					redraw();
					return;
				case "tab":
					await complete();
					return;
				default:
					editor.apply(key);
					redraw();
			}
		};

		// Keys wait their turn, so a pasted script runs line by line. While a
		// command runs, only Ctrl-C is heard: it cancels the request.
		let queue = Promise.resolve();
		const data = term.onData((input) => {
			for (const key of parseKeys(input)) {
				if (running) {
					if (key.type === "interrupt") {
						running.abort();
						term.write("^C\r\n");
					}
					continue;
				}
				queue = queue.then(() => handle(key));
			}
		});

		term.write(
			`${dim("TinyStack CloudShell. The host AWS CLI runs as your console identity.")}\r\n` +
				`${dim("Type help for the built-ins (clear, history, whoami, echo), Tab to complete, Ctrl-C to cancel.")}\r\n\r\n`,
		);
		redraw();

		const resize = new ResizeObserver(() => fit.fit());
		resize.observe(host);
		const theme = new MutationObserver(() => {
			term.options.theme = isDark() ? dark : light;
		});
		theme.observe(document.body, {
			attributes: true,
			attributeFilter: ["class"],
		});

		return () => {
			stopped = true;
			running?.abort();
			data.dispose();
			resize.disconnect();
			theme.disconnect();
			term.dispose();
		};
	}, []);

	return (
		<>
			{banner && (
				<div
					role="alert"
					style={{ padding: 12, background: "#fff3cd", color: "#664d03" }}
				>
					{banner}{" "}
					<a
						href="https://amjadjibon.github.io/tinystack/guides/cloudshell/"
						target="_blank"
						rel="noreferrer"
					>
						CloudShell setup
					</a>
				</div>
			)}
			<div
				ref={container}
				role="application"
				aria-label="CloudShell terminal"
				style={{ height: "max(360px, calc(100vh - 340px))", width: "100%" }}
			/>
		</>
	);
}
