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
	cleanup: string[];
};
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
