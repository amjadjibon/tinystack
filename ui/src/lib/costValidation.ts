type Definition = Record<string, unknown>;
export function required(value: string, label: string, max = 1024) {
	if (!value || [...value].length > max)
		throw new Error(`${label} requires 1–${max} characters.`);
	return value;
}
export function budgetName(value: string) {
	required(value, "Budget name", 100);
	if (
		/[:\\]/.test(value) ||
		value.includes("/action/") ||
		/<script>[\s\S]*<\/script>/i.test(value)
	)
		throw new Error("Budget name contains unsupported characters.");
	return value;
}
export function spend(amount: string, unit: string) {
	if (!/^(\d*\.)?\d+$/.test(amount))
		throw new Error("Limit must be a nonnegative decimal.");
	return { Amount: amount, Unit: required(unit, "Unit") };
}
export function jsonArray(
	text: string,
	label: string,
	min = 0,
	max = 500,
): Definition[] {
	let value: unknown;
	try {
		value = JSON.parse(text);
	} catch {
		throw new Error(`${label} must be valid JSON.`);
	}
	if (
		!Array.isArray(value) ||
		value.length < min ||
		value.length > max ||
		value.some((v) => !v || typeof v !== "object" || Array.isArray(v))
	)
		throw new Error(`${label} must be a JSON array of ${min}–${max} objects.`);
	return value;
}
export function subscriberAddresses(text: string, type: string) {
	const addresses = text
		.split("\n")
		.map((v) => v.trim())
		.filter(Boolean);
	if (!addresses.length || new Set(addresses).size !== addresses.length)
		throw new Error("Provide distinct subscriber addresses, one per line.");
	for (const address of addresses) {
		if (
			type === "SNS"
				? !/^arn:aws[-a-z]*:sns:[a-z0-9-]+:\d{12}:[\w-]+(?:\.fifo)?$/.test(
						address,
					)
				: !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(address)
		)
			throw new Error(`Invalid ${type} subscriber address: ${address}`);
	}
	return addresses;
}
export function notification(
	type: string,
	comparison: string,
	threshold: string,
	thresholdType: string,
): Definition {
	if (
		!threshold.trim() ||
		!Number.isFinite(Number(threshold)) ||
		Number(threshold) < 0 ||
		Number(threshold) > 15000000000000
	)
		throw new Error("Threshold must be between 0 and 15000000000000.");
	return {
		NotificationType: type,
		ComparisonOperator: comparison,
		Threshold: Number(threshold),
		ThresholdType: thresholdType,
	};
}
export function categoryName(value: string) {
	required(value, "Category name or value", 50);
	if (value !== value.trim() || !/^[\p{L}\p{N}\p{Z}_-]+$/u.test(value))
		throw new Error(
			"Category names and values allow letters, numbers, spaces, underscores and hyphens without surrounding spaces.",
		);
	return value;
}

export function expression(
	value: Definition,
	purpose: "threshold" | "category" | "monitor",
) {
	const roots = Object.keys(value).filter(
		(k) => !Array.isArray(value[k]) || (value[k] as unknown[]).length,
	);
	if (roots.length !== 1)
		throw new Error("Each expression requires exactly one root operator.");
	const root = roots[0];
	if (root === "And" || root === "Or") {
		if (purpose === "monitor")
			throw new Error(
				"Monitor specifications require Dimensions, Tags or CostCategories.",
			);
		if (!Array.isArray(value[root]) || !(value[root] as unknown[]).length)
			throw new Error(`${root} requires expressions.`);
		for (const child of value[root] as Definition[]) expression(child, purpose);
	} else if (root === "Not") {
		if (purpose !== "category") throw new Error("Not is not supported here.");
		expression(value.Not as Definition, purpose);
	} else {
		if (
			!["Dimensions", "Tags", "CostCategories"].includes(root) ||
			!value[root] ||
			typeof value[root] !== "object" ||
			Array.isArray(value[root])
		)
			throw new Error("Invalid expression root.");
		const field = value[root] as {
			Key?: string;
			Values?: string[];
			MatchOptions?: string[];
		};
		if (typeof field.Key !== "string" || [...field.Key].length > 1024)
			throw new Error(
				"Expression key must be a string of at most 1024 characters.",
			);
		if (
			field.Values !== undefined &&
			(!Array.isArray(field.Values) ||
				field.Values.some((v) => typeof v !== "string" || [...v].length > 1024))
		)
			throw new Error(
				"Expression values must be strings of at most 1024 characters.",
			);
		if (
			field.MatchOptions !== undefined &&
			(!Array.isArray(field.MatchOptions) ||
				field.MatchOptions.some(
					(v) =>
						![
							"EQUALS",
							"ABSENT",
							"STARTS_WITH",
							"ENDS_WITH",
							"CONTAINS",
							"CASE_SENSITIVE",
							"CASE_INSENSITIVE",
							"GREATER_THAN_OR_EQUAL",
						].includes(v),
				))
		)
			throw new Error("Invalid expression match option.");
		if (
			purpose === "threshold" &&
			(root !== "Dimensions" ||
				![
					"ANOMALY_TOTAL_IMPACT_ABSOLUTE",
					"ANOMALY_TOTAL_IMPACT_PERCENTAGE",
				].includes(field.Key) ||
				field.Values?.length !== 1 ||
				!field.Values[0].trim() ||
				!Number.isFinite(Number(field.Values[0])) ||
				Number(field.Values[0]) < 0 ||
				field.MatchOptions?.length !== 1 ||
				field.MatchOptions[0] !== "GREATER_THAN_OR_EQUAL")
		)
			throw new Error(
				"Threshold requires an impact dimension, one finite nonnegative value and GREATER_THAN_OR_EQUAL.",
			);
		if (
			purpose === "category" &&
			root === "Dimensions" &&
			![
				"LINKED_ACCOUNT",
				"SERVICE_CODE",
				"RECORD_TYPE",
				"LINKED_ACCOUNT_NAME",
				"REGION",
				"USAGE_TYPE",
			].includes(field.Key)
		)
			throw new Error("Unsupported cost category dimension.");
	}
	return value;
}
export function monitorSpecification(
	value: Definition,
	type: string,
	dimension: string,
) {
	expression(value, "monitor");
	const root = Object.keys(value)[0];
	const field = value[root] as {
		Key: string;
		Values?: string[];
		MatchOptions?: string[];
	};
	if (field.MatchOptions?.length)
		throw new Error("Monitor specifications cannot contain match options.");
	if (type === "DIMENSIONAL") {
		if (
			root !== (dimension === "TAG" ? "Tags" : "CostCategories") ||
			!field.Key ||
			field.Values?.length
		)
			throw new Error(
				"Dimensional TAG/COST_CATEGORY monitors require only the corresponding key.",
			);
	} else {
		if (
			(root === "Dimensions" && field.Key !== "LINKED_ACCOUNT") ||
			!field.Key ||
			!field.Values?.length ||
			field.Values.length > (root === "CostCategories" ? 1 : 10)
		)
			throw new Error(
				"CUSTOM monitors require 1–10 account/tag values or one cost category value.",
			);
	}
	return value;
}
