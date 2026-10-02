import { createContext, type ReactNode, useContext } from "react";
import { api, type Health } from "./api";
import { type Poll, usePoll } from "./usePoll";

const HealthContext = createContext<Poll<Health> | undefined>(undefined);

// HealthProvider polls /_tinystack/health once for the whole console.
export function HealthProvider({ children }: { children: ReactNode }) {
	const health = usePoll(api.health, 15000);
	return (
		<HealthContext.Provider value={health}>{children}</HealthContext.Provider>
	);
}

export function useHealth(): Poll<Health> {
	const health = useContext(HealthContext);
	if (!health) throw new Error("useHealth outside HealthProvider");
	return health;
}
