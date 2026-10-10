import { useSyncExternalStore } from "react";

const key = "tinystack.console.favorite-services";
const none: string[] = [];
let cached: { raw: string | null; names: string[] } = {
	raw: null,
	names: none,
};
const listeners = new Set<() => void>();

function read(): string[] {
	let raw: string | null = null;
	try {
		raw = localStorage.getItem(key);
	} catch {
		// Favorites need browser storage; without it there are none.
	}

	if (raw === cached.raw) return cached.names;
	let names = none;
	try {
		const saved = JSON.parse(raw ?? "[]");
		if (Array.isArray(saved))
			names = saved.filter((name) => typeof name === "string");
	} catch {
		// A damaged entry reads as no favorites.
	}

	cached = { raw, names };
	return names;
}

function subscribe(listener: () => void) {
	listeners.add(listener);
	// Other tabs change favorites through the storage event.
	window.addEventListener("storage", listener);
	return () => {
		listeners.delete(listener);
		window.removeEventListener("storage", listener);
	};
}

// useFavorites returns the starred services, in the order they were starred.
export function useFavorites() {
	return useSyncExternalStore(subscribe, read, () => none);
}

// toggleFavorite stars a service, or unstars it if it already is.
export function toggleFavorite(name: string) {
	const names = read();
	const next = names.includes(name)
		? names.filter((item) => item !== name)
		: [...names, name];
	try {
		localStorage.setItem(key, JSON.stringify(next));
	} catch {
		return;
	}

	for (const listener of listeners) listener();
}
