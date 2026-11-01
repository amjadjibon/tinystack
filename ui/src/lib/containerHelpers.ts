export async function containerPages<T>(
	call: (
		operation: string,
		input: Record<string, unknown>,
	) => Promise<Record<string, unknown>>,
	operation: string,
	key: string,
	input: Record<string, unknown> = {},
): Promise<T[]> {
	const items: T[] = [],
		seen = new Set<string>();
	let nextToken: string | undefined;
	do {
		const out = await call(operation, {
			...input,
			maxResults: 100,
			...(nextToken ? { nextToken } : {}),
		});
		items.push(...((out[key] as T[] | undefined) ?? []));
		nextToken = out.NextToken as string | undefined;
		if (nextToken && seen.has(nextToken))
			throw new Error("Repeated pagination token.");
		if (nextToken) seen.add(nextToken);
	} while (nextToken);
	return items;
}
export function jsonObject(text: string): Record<string, unknown> {
	const value: unknown = JSON.parse(text);
	if (!value || typeof value !== "object" || Array.isArray(value))
		throw new Error("Enter a JSON object.");
	return value as Record<string, unknown>;
}
export function taskDefinitionInput(text: string) {
	const value = jsonObject(text);
	if (typeof value.family !== "string" || !/^[\w-]{1,255}$/.test(value.family))
		throw new Error(
			"Family must contain 1–255 letters, numbers, underscores or hyphens.",
		);
	if (
		!Array.isArray(value.containerDefinitions) ||
		!value.containerDefinitions.length
	)
		throw new Error("Add at least one container definition.");
	const names = new Set<string>();
	for (const c of value.containerDefinitions) {
		if (
			!c ||
			typeof c !== "object" ||
			typeof c.name !== "string" ||
			!c.name ||
			typeof c.image !== "string" ||
			!c.image
		)
			throw new Error("Each container needs a name and image.");
		if (names.has(c.name)) throw new Error("Container names must be unique.");
		names.add(c.name);
		if (!value.memory && !c.memory && !c.memoryReservation)
			throw new Error(
				`Container ${c.name} needs memory or memoryReservation, or set task memory.`,
			);
		for (const key of ["memory", "memoryReservation"]) {
			if (
				c[key] !== undefined &&
				(typeof c[key] !== "number" ||
					!Number.isSafeInteger(c[key]) ||
					c[key] <= 0)
			)
				throw new Error(
					`Container ${c.name}: ${key} must be a positive whole number.`,
				);
		}
		if (
			typeof c.memory === "number" &&
			typeof c.memoryReservation === "number" &&
			c.memoryReservation > c.memory
		)
			throw new Error("Memory reservation cannot exceed container memory.");
	}
	return value;
}
export function desiredCount(value: string, min = 0) {
	const n = Number(value);
	if (!value.trim() || !Number.isSafeInteger(n) || n < min)
		throw new Error(`Enter a whole number of at least ${min}.`);
	return n;
}
