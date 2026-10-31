import { clearProgress } from "../tutorials/progress";
import { adminRequest } from "./adminApi";
import {
	type FaultRule,
	faultRequest,
	type InitStatus,
} from "./developerTools";
export const developer = {
	faults: () => adminRequest<FaultRule[]>("/_tinystack/faults"),
	effects: () =>
		adminRequest<{ latency: number }>("/_localstack/chaos/effects"),
	init: () => adminRequest<InitStatus>("/_tinystack/init"),
	reset: async (init: boolean) => {
		const result = await adminRequest(
			`/_tinystack/reset${init ? "?init=1" : ""}`,
			"POST",
		);
		try {
			clearProgress(localStorage);
		} catch {
			/* Storage may be unavailable. */
		}
		window.dispatchEvent(new Event("tutorial-reset"));
		return result;
	},
	changeFaults: (method: "PATCH" | "DELETE", rules: FaultRule[]) => {
		const request = faultRequest(method, rules);
		return adminRequest(request.path, request.method, request.body);
	},
	setLatency: (latency: number) =>
		adminRequest("/_localstack/chaos/effects", "POST", { latency }),
};
