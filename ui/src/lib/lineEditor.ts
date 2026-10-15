// A small line editor for the CloudShell terminal: the text being typed, the
// cursor in it, and the commands typed before. It knows nothing about xterm.js,
// so it runs in tests.

export type Key =
	| { type: "text"; text: string }
	| {
			type:
				| "enter"
				| "backspace"
				| "delete"
				| "left"
				| "right"
				| "home"
				| "end"
				| "up"
				| "down"
				| "tab"
				| "interrupt"
				| "clear"
				| "killToStart"
				| "killToEnd"
				| "killWord";
	  };

const controls: Record<string, Key> = {
	"\r": { type: "enter" },
	"\n": { type: "enter" },
	"\x7f": { type: "backspace" },
	"\b": { type: "backspace" },
	"\t": { type: "tab" },
	"\x03": { type: "interrupt" },
	"\x0c": { type: "clear" },
	"\x01": { type: "home" },
	"\x05": { type: "end" },
	"\x15": { type: "killToStart" },
	"\x0b": { type: "killToEnd" },
	"\x17": { type: "killWord" },
	"\x02": { type: "left" },
	"\x06": { type: "right" },
	"\x10": { type: "up" },
	"\x0e": { type: "down" },
};

const sequences: Record<string, Key> = {
	"[A": { type: "up" },
	"[B": { type: "down" },
	"[C": { type: "right" },
	"[D": { type: "left" },
	"[H": { type: "home" },
	"[F": { type: "end" },
	OH: { type: "home" },
	OF: { type: "end" },
	"[1~": { type: "home" },
	"[4~": { type: "end" },
	"[3~": { type: "delete" },
};

// parseKeys splits the data a terminal sends, which may be one key or a whole
// paste, into keys. Unknown escape sequences are dropped.
export function parseKeys(data: string): Key[] {
	const keys: Key[] = [];
	let text = "";
	const flush = () => {
		if (text) keys.push({ type: "text", text });
		text = "";
	};
	for (let i = 0; i < data.length; i++) {
		const c = data[i];
		if (c === "\x1b") {
			flush();
			const rest = data.slice(i + 1);
			const match = Object.keys(sequences)
				.sort((a, b) => b.length - a.length)
				.find((s) => rest.startsWith(s));
			if (match) {
				keys.push(sequences[match]);
				i += match.length;
				continue;
			}
			// Skip a CSI sequence we do not know: ESC [ params final-byte.
			const csi = /^\[[0-9;?]*[ -/]*[@-~]/.exec(rest);
			if (csi) i += csi[0].length;
			continue;
		}
		const control = controls[c];
		if (control) {
			flush();
			keys.push(control);
		} else if (c >= " ") {
			text += c;
		}
	}
	flush();
	return keys;
}

export class LineEditor {
	buffer = "";
	cursor = 0;
	history: string[];
	private position: number;
	private draft = "";

	constructor(history: string[] = []) {
		this.history = [...history];
		this.position = this.history.length;
	}

	insert(text: string) {
		this.buffer =
			this.buffer.slice(0, this.cursor) + text + this.buffer.slice(this.cursor);
		this.cursor += text.length;
	}

	// replaceWord swaps the word before the cursor for text.
	replaceWord(text: string) {
		const start = this.wordStart();
		this.buffer =
			this.buffer.slice(0, start) + text + this.buffer.slice(this.cursor);
		this.cursor = start + text.length;
	}

	// word is the text from the last space before the cursor to the cursor.
	word() {
		return this.buffer.slice(this.wordStart(), this.cursor);
	}

	private wordStart() {
		const before = this.buffer.slice(0, this.cursor);
		return Math.max(before.lastIndexOf(" "), before.lastIndexOf("\t")) + 1;
	}

	backspace() {
		if (this.cursor === 0) return;
		this.buffer =
			this.buffer.slice(0, this.cursor - 1) + this.buffer.slice(this.cursor);
		this.cursor--;
	}

	delete() {
		this.buffer =
			this.buffer.slice(0, this.cursor) + this.buffer.slice(this.cursor + 1);
	}

	left() {
		this.cursor = Math.max(0, this.cursor - 1);
	}

	right() {
		this.cursor = Math.min(this.buffer.length, this.cursor + 1);
	}

	home() {
		this.cursor = 0;
	}

	end() {
		this.cursor = this.buffer.length;
	}

	killToStart() {
		this.buffer = this.buffer.slice(this.cursor);
		this.cursor = 0;
	}

	killToEnd() {
		this.buffer = this.buffer.slice(0, this.cursor);
	}

	killWord() {
		const before = this.buffer.slice(0, this.cursor).trimEnd();
		const start =
			Math.max(before.lastIndexOf(" "), before.lastIndexOf("\t")) + 1;
		this.buffer = this.buffer.slice(0, start) + this.buffer.slice(this.cursor);
		this.cursor = start;
	}

	up() {
		if (this.position === 0) return;
		if (this.position === this.history.length) this.draft = this.buffer;
		this.position--;
		this.show(this.history[this.position]);
	}

	down() {
		if (this.position >= this.history.length) return;
		this.position++;
		this.show(
			this.position === this.history.length
				? this.draft
				: this.history[this.position],
		);
	}

	private show(text: string) {
		this.buffer = text;
		this.cursor = text.length;
	}

	// clearLine drops what was typed, as Ctrl-C does.
	clearLine() {
		this.show("");
		this.position = this.history.length;
	}

	// forget drops the history, as history -c does.
	forget() {
		this.history = [];
		this.position = 0;
	}

	// submit returns the line, remembers it unless it is blank or repeats the
	// last, and starts a new one.
	submit(limit = 200) {
		const line = this.buffer;
		if (line.trim() && this.history.at(-1) !== line) {
			this.history.push(line);
			if (this.history.length > limit)
				this.history.splice(0, this.history.length - limit);
		}
		this.clearLine();
		return line;
	}

	// apply performs an editing key; the keys that need the terminal or the
	// server (enter, tab, interrupt, clear) are left to the caller.
	apply(key: Key) {
		switch (key.type) {
			case "text":
				this.insert(key.text);
				break;
			case "backspace":
				this.backspace();
				break;
			case "delete":
				this.delete();
				break;
			case "left":
				this.left();
				break;
			case "right":
				this.right();
				break;
			case "home":
				this.home();
				break;
			case "end":
				this.end();
				break;
			case "up":
				this.up();
				break;
			case "down":
				this.down();
				break;
			case "killToStart":
				this.killToStart();
				break;
			case "killToEnd":
				this.killToEnd();
				break;
			case "killWord":
				this.killWord();
				break;
			default:
				break;
		}
	}
}

// commonPrefix is the longest start all the words share.
export function commonPrefix(words: string[]) {
	if (words.length === 0) return "";
	let prefix = words[0];
	for (const word of words) {
		while (!word.startsWith(prefix)) prefix = prefix.slice(0, -1);
	}
	return prefix;
}

// columns lays words out in rows that fit a terminal of the given width.
export function columns(words: string[], width: number) {
	const cell = Math.max(...words.map((w) => w.length)) + 2;
	const perRow = Math.max(1, Math.floor(width / cell));
	const rows: string[] = [];
	for (let i = 0; i < words.length; i += perRow) {
		rows.push(
			words
				.slice(i, i + perRow)
				.map((w) => w.padEnd(cell))
				.join("")
				.trimEnd(),
		);
	}
	return rows;
}
