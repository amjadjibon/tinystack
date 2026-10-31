// Keep request construction independent of React and the console transport.
export function dynamoName(value: string) {
	if (!/^[A-Za-z0-9_.-]{3,255}$/.test(value))
		throw new Error(
			"Names require 3–255 letters, numbers, underscores, hyphens or periods.",
		);
	return value;
}
export function batchChunks<T>(items: T[]): T[][] {
	return Array.from({ length: Math.ceil(items.length / 25) }, (_, i) =>
		items.slice(i * 25, i * 25 + 25),
	);
}
export function statementInput(
	statement: string,
	parameters: string,
	limit: string,
) {
	if (!statement.trim()) throw new Error("Enter a PartiQL statement.");
	const parsed: unknown = JSON.parse(parameters);
	if (!Array.isArray(parsed))
		throw new Error("Parameters must be a DynamoDB JSON array.");
	const n = Number(limit);
	if (!Number.isInteger(n) || n < 1 || n > 1000)
		throw new Error("Page limit must be 1–1,000.");
	return { Statement: statement, Parameters: parsed, Limit: n };
}
