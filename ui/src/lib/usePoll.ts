import { useCallback, useEffect, useRef, useState } from "react";

export type Poll<T> = {
	data: T | undefined;
	error: string | undefined;
	loading: boolean;
	reload: () => Promise<void>;
};

// usePoll loads fetcher now and then every intervalMs (never when it is 0)
// while the page is visible, reloading as soon as it is shown again. It keeps
// the last good data while a later load fails.
export function usePoll<T>(
	fetcher: () => Promise<T>,
	intervalMs = 5000,
): Poll<T> {
	const [data, setData] = useState<T>();
	const [error, setError] = useState<string>();
	const [loading, setLoading] = useState(true);
	const fetcherRef = useRef(fetcher);
	fetcherRef.current = fetcher;

	const reload = useCallback(async () => {
		try {
			setData(await fetcherRef.current());
			setError(undefined);
		} catch (err) {
			setError(err instanceof Error ? err.message : String(err));
		} finally {
			setLoading(false);
		}
	}, []);

	useEffect(() => {
		reload();
		if (intervalMs <= 0) return;

		const timer = setInterval(() => {
			if (document.visibilityState === "visible") reload();
		}, intervalMs);
		const onVisible = () => {
			if (document.visibilityState === "visible") reload();
		};
		document.addEventListener("visibilitychange", onVisible);
		return () => {
			clearInterval(timer);
			document.removeEventListener("visibilitychange", onVisible);
		};
	}, [reload, intervalMs]);

	return { data, error, loading, reload };
}
