export type FaultRule = {
	service?: string;
	operation?: string;
	region?: string;
	account?: string;
	probability?: number;
	latencyMs?: number;
	error?: { code: string; message?: string; statusCode?: number };
};
export const faultFields = [
	"service",
	"operation",
	"region",
	"account",
	"code",
	"message",
	"statusCode",
	"probability",
	"latencyMs",
] as const;
export type FaultForm = Record<(typeof faultFields)[number], string>;
export const emptyFaultForm: FaultForm = {
	service: "",
	operation: "",
	region: "",
	account: "",
	code: "",
	message: "",
	statusCode: "",
	probability: "",
	latencyMs: "",
};
export function latencyValue(text: string) {
	if (
		!/^\d+$/.test(text) ||
		!Number.isSafeInteger(Number(text)) ||
		Number(text) > 3600000
	)
		throw new Error("latencyMs must be between 0 and 3600000");
	return Number(text);
}
export function faultRule(form: FaultForm, count = 0): FaultRule {
	if (count >= 1024) throw new Error("faults permits at most 1024 rules");
	const rule: FaultRule = {};
	for (const field of ["service", "operation"] as const) {
		if (form[field] && !/^[A-Za-z0-9_.-]+$/.test(form[field]))
			throw new Error(
				`${field} must be an exact name without whitespace or wildcards`,
			);
		if (form[field]) rule[field] = form[field];
	}
	if (form.region && !/^[a-z]{2}(?:-[a-z]+)+-\d+$/.test(form.region))
		throw new Error("region must be an AWS Region name");
	if (form.account && !/^\d{12}$/.test(form.account))
		throw new Error("account must have 12 digits");
	if (form.region) rule.region = form.region;
	if (form.account) rule.account = form.account;
	if (form.latencyMs) rule.latencyMs = latencyValue(form.latencyMs);
	if (form.probability) {
		const p = Number(form.probability);
		if (!form.probability.trim() || !Number.isFinite(p) || p < 0 || p > 1)
			throw new Error("probability must be between 0 and 1");
		rule.probability = p;
	}
	if (form.code || form.message || form.statusCode) {
		if (!/^[A-Za-z0-9_.-]+$/.test(form.code))
			throw new Error("error.code must be a nonempty AWS error name");
		rule.error = { code: form.code };
		if (form.message) rule.error.message = form.message;
		if (form.statusCode) {
			const status = Number(form.statusCode);
			if (
				!/^\d+$/.test(form.statusCode) ||
				(status !== 0 && (status < 400 || status > 599))
			)
				throw new Error("error.statusCode must be between 400 and 599");
			rule.error.statusCode = status;
		}
	}
	if (!rule.error && !rule.latencyMs)
		throw new Error("an error or positive latencyMs is required");
	return rule;
}
export function faultRequest(method: "PATCH" | "DELETE", rules: FaultRule[]) {
	return { path: "/_tinystack/faults", method, body: rules };
}
export type InitScript = {
	stage: string;
	name: string;
	path: string;
	state: string;
	exit_code?: number;
	error?: string;
	started_at?: string;
	finished_at?: string;
	output?: string;
};
export type InitStatus = {
	completed: Record<string, boolean>;
	stages: Record<string, string>;
	scripts: InitScript[];
};
export function initPending(status?: InitStatus) {
	return (
		!!status &&
		(Object.values(status.stages).includes("RUNNING") ||
			status.scripts.some(
				(s) => s.state === "UNKNOWN" || s.state === "RUNNING",
			))
	);
}
