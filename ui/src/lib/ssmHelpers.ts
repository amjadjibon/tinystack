export function parameterFilters(
	prefix: string,
	mode: string,
	type: string,
	tier: string,
) {
	return [
		...(prefix
			? [
					{
						Key: mode === "prefix" ? "Name" : "Path",
						Option: mode === "prefix" ? "BeginsWith" : mode,
						Values: [prefix],
					},
				]
			: []),
		...(type ? [{ Key: "Type", Option: "Equals", Values: [type] }] : []),
		...(tier ? [{ Key: "Tier", Option: "Equals", Values: [tier] }] : []),
	];
}
// Values belong only in the explicit reveal component, never in paginated metadata.
export function parameterMetadata<T extends { Value?: string }>(
	item: T,
): Omit<T, "Value"> {
	const { Value: _value, ...metadata } = item;
	return metadata;
}
