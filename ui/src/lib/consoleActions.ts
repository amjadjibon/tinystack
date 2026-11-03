export type ActionStep = { label: string; run: () => Promise<unknown> };
export function errorText(error: unknown) {
	return error instanceof Error ? error.message : String(error);
}
export function jsonObject(
	text: string,
	label: string,
): Record<string, unknown> {
	let value: unknown;
	try {
		value = JSON.parse(text);
	} catch {
		throw new Error(`${label} must be valid JSON.`);
	}
	if (!value || typeof value !== "object" || Array.isArray(value))
		throw new Error(`${label} must be a JSON object.`);
	return value as Record<string, unknown>;
}
export function stringMap(text: string, label: string): Record<string, string> {
	const value = jsonObject(text, label);
	if (Object.values(value).some((v) => typeof v !== "string"))
		throw new Error(`${label} values must be strings.`);
	return value as Record<string, string>;
}
// words splits a list a person typed with commas, spaces or new lines.
export const words = (text: string) => text.split(/[\s,]+/).filter(Boolean);
export function integer(text: string, label: string, min: number, max: number) {
	if (
		!/^\d+$/.test(text) ||
		!Number.isSafeInteger(Number(text)) ||
		Number(text) < min ||
		Number(text) > max
	)
		throw new Error(`${label} must be an integer from ${min} to ${max}.`);
	return Number(text);
}
