import type { ActionStep } from "../lib/consoleActions.ts";
import type { Check, Context } from "./checks.ts";
import type { HotspotId } from "./hotspots.ts";
export type Step = {
	title: string;
	content: string;
	command: string;
	hotspotId: HotspotId;
	route: string;
	check: Check;
};
export type Tutorial = {
	id: string;
	title: string;
	description: string;
	services: string[];
	duration: string;
	prerequisites: string;
	learnMore: string;
	tasks: { title: string; steps: Step[] }[];
	/** cleanup lists the generated resources to delete, in order; each run fails explicitly rather than widening. */
	cleanup: (c: Context, call: Call) => Promise<ActionStep[]>;
};
export type Call = <T>(
	service: string,
	operation: string,
	input: Record<string, unknown>,
) => Promise<T>;
export function expand(text: string, c: Context) {
	return text.replace(
		/\{(\w+)\}/g,
		(_, key: string) =>
			c.names[key] ??
			(key === "account"
				? c.account
				: key === "region"
					? c.region
					: `{${key}}`),
	);
}
