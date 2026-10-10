import { useLocation, useNavigate } from "@tanstack/react-router";
import {
	createContext,
	type ReactNode,
	useContext,
	useEffect,
	useState,
} from "react";
import { api } from "./api";
import { validRegion } from "./s3";
import { usePoll } from "./usePoll";

type Scope = { account: string; region: string };
const storageKey = "tinystack-console-scope";
const globalServices = new Set(["iam", "route53", "cloudfront"]);

function rememberedScope(): Scope {
	try {
		const saved = JSON.parse(localStorage.getItem(storageKey) ?? "{}");
		return {
			account:
				typeof saved.account === "string" && /^\d{12}$/.test(saved.account)
					? saved.account
					: "",
			region:
				typeof saved.region === "string" && validRegion(saved.region)
					? saved.region
					: "",
		};
	} catch {
		return { account: "", region: "" };
	}
}

function useScopeState() {
	const location = useLocation();
	const navigate = useNavigate();
	const capabilities = usePoll(api.actionCapabilities, 15000);
	const inventory = usePoll(api.resources);
	const [remembered, setRemembered] = useState<Scope>({
		account: "",
		region: "",
	});
	useEffect(() => setRemembered(rememberedScope()), []);
	const search = location.search as Record<string, unknown>;
	const routeAccount = String(search.account ?? "");
	const routeRegion = typeof search.region === "string" ? search.region : "";
	const account =
		routeAccount || remembered.account || capabilities.data?.account || "";
	const region =
		(validRegion(routeRegion) ? routeRegion : "") ||
		remembered.region ||
		capabilities.data?.region ||
		"";
	const name = location.pathname.split("/")[2];
	const global =
		location.pathname.startsWith("/services/") && globalServices.has(name);

	// Deep links and browser history also update the scope used by the next service.
	useEffect(() => {
		if (!/^\d{12}$/.test(account) || !validRegion(region)) return;
		setRemembered((previous) =>
			previous.account === account && previous.region === region
				? previous
				: { account, region },
		);
		try {
			localStorage.setItem(storageKey, JSON.stringify({ account, region }));
		} catch {
			// The current session still works when browser storage is unavailable.
		}
	}, [account, region]);

	const switchScope = (next: Partial<Scope>) => {
		const scope = { account, region, ...next };
		if (scope.account === account && scope.region === region) return;
		if (!/^\d{12}$/.test(scope.account) || !validRegion(scope.region)) return;
		setRemembered(scope);
		const params = new URLSearchParams(location.searchStr);
		params.set("account", scope.account);
		if (!global) params.set("region", scope.region);
		else params.delete("region");
		// A resource belongs to the old scope. Return to its list when switching.
		for (const key of [
			"name",
			"type",
			"create",
			"bucket",
			"secret",
			"database",
			"queue",
			"topic",
			"subscription",
			"function",
			"table",
			"bus",
			"rule",
			"group",
			"stream",
			"alarm",
			"dashboard",
			"machine",
			"id",
			"vpc",
			"execution",
			"apiId",
			"resourceId",
			"method",
			"prefix",
			"key",
			"version",
		])
			params.delete(key);
		const pathname = location.pathname.endsWith("/resource")
			? location.pathname.slice(0, -9)
			: location.pathname;
		navigate({ href: `${pathname}?${params}` });
	};
	return {
		account,
		region,
		global,
		switchScope,
		capabilities,
		inventory,
		accounts: [
			...new Set([
				account,
				capabilities.data?.account,
				...(inventory.data ?? []).map((r) => r.account),
			]),
		]
			.filter((v): v is string => !!v)
			.sort(),
		regions: [
			...new Set([
				region,
				capabilities.data?.region,
				...(inventory.data ?? []).map((r) => r.region),
			]),
		]
			.filter((v): v is string => !!v && validRegion(v))
			.sort(),
	};
}

const ScopeContext = createContext<ReturnType<typeof useScopeState> | null>(
	null,
);

export function ScopeProvider({ children }: { children: ReactNode }) {
	const scope = useScopeState();
	return (
		<ScopeContext.Provider value={scope}>{children}</ScopeContext.Provider>
	);
}

export function useScope() {
	const scope = useContext(ScopeContext);
	if (!scope) throw new Error("ScopeProvider is required");
	return scope;
}
