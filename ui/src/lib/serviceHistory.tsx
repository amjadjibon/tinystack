import { useLocation } from "@tanstack/react-router";
import {
	createContext,
	type ReactNode,
	useContext,
	useEffect,
	useState,
} from "react";

const key = "tinystack.console.recent-services";
const ServiceHistory = createContext<string[]>([]);

export function ServiceHistoryProvider({ children }: { children: ReactNode }) {
	const { pathname } = useLocation();
	const [recent, setRecent] = useState<string[]>([]);
	useEffect(() => {
		try {
			const saved = JSON.parse(localStorage.getItem(key) ?? "[]");
			if (Array.isArray(saved))
				setRecent(saved.filter((name) => typeof name === "string").slice(0, 8));
		} catch {
			// Visiting services still works without browser storage.
		}
	}, []);
	useEffect(() => {
		const name = pathname.match(/^\/services\/([^/]+)/)?.[1];
		if (!name) return;
		setRecent((previous) => {
			const next = [name, ...previous.filter((item) => item !== name)].slice(
				0,
				8,
			);
			try {
				localStorage.setItem(key, JSON.stringify(next));
			} catch {
				// Keep the current session's history when storage is unavailable.
			}
			return next;
		});
	}, [pathname]);
	return (
		<ServiceHistory.Provider value={recent}>{children}</ServiceHistory.Provider>
	);
}

export function useServiceHistory() {
	return useContext(ServiceHistory);
}
