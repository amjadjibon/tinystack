export type Progress = {
	step: number;
	completed: number[];
	names: Record<string, string>;
	finishedAt?: string;
};
export type Store = Pick<
	Storage,
	"getItem" | "setItem" | "removeItem" | "key" | "length"
>;
const prefix = "tinystack.tutorials.";
const key = (id: string, account: string, region: string) =>
	prefix + JSON.stringify([id, account, region]);
export function load(
	store: Store,
	id: string,
	account: string,
	region: string,
): Progress | null {
	try {
		const p = JSON.parse(store.getItem(key(id, account, region)) ?? "null");
		return p &&
			Number.isInteger(p.step) &&
			p.step >= 0 &&
			Array.isArray(p.completed) &&
			p.names &&
			Object.values(p.names).every((n) => typeof n === "string")
			? p
			: null;
	} catch {
		return null;
	}
}
export function save(
	store: Store,
	id: string,
	account: string,
	region: string,
	p: Progress,
) {
	store.setItem(key(id, account, region), JSON.stringify(p));
}
export function restart(
	store: Store,
	id: string,
	account: string,
	region: string,
) {
	store.removeItem(key(id, account, region));
}
export function clearProgress(store: Store) {
	for (let i = store.length - 1; i >= 0; i--) {
		const k = store.key(i);
		if (k?.startsWith(prefix)) store.removeItem(k);
	}
}
export function fresh(id: string): Progress {
	const random = globalThis.crypto.randomUUID().replaceAll("-", "").slice(0, 4);
	const name = `tutorial-${id}-${random}`;
	return {
		step: 0,
		completed: [],
		names: {
			bucket: name,
			key: "hello.txt",
			topic: name,
			queue: name,
			table: name,
			rule: name,
			function: name,
			machine: name,
			execution: name,
			secret: name,
			parameter: name,
			role: name,
			policy: name,
			pool: name,
			client: name,
			username: name,
			email: `${name}@example.test`,
			stack: name,
		},
	};
}
