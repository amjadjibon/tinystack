export type Trail = {
	Name: string;
	TrailARN?: string;
	S3BucketName?: string;
	S3KeyPrefix?: string;
	HomeRegion?: string;
};
export type TrailForm = {
	name: string;
	bucket: string;
	prefix: string;
	tags: string;
};
export function trailForm(t?: Trail): TrailForm {
	return {
		name: t?.Name ?? "",
		bucket: t?.S3BucketName ?? "",
		prefix: t?.S3KeyPrefix ?? "",
		tags: "{}",
	};
}
export function trailInput(
	f: TrailForm,
	creating: boolean,
): Record<string, unknown> {
	if (!/^[a-zA-Z0-9][a-zA-Z0-9._-]{1,126}[a-zA-Z0-9]$/.test(f.name.trim()))
		throw new Error(
			"Trail name must be 3–128 characters, start and end with a letter or digit, and contain only letters, digits, periods, underscores or hyphens.",
		);
	if (!f.bucket.trim()) throw new Error("An existing S3 bucket is required.");
	const input: Record<string, unknown> = {
		Name: f.name.trim(),
		S3BucketName: f.bucket.trim(),
		S3KeyPrefix: f.prefix,
	};
	if (creating) {
		const tags: unknown = JSON.parse(f.tags);
		if (
			!tags ||
			typeof tags !== "object" ||
			Array.isArray(tags) ||
			Object.values(tags).some((v) => typeof v !== "string")
		)
			throw new Error("Tags must be a JSON object of string values.");
		input.TagsList = Object.entries(tags).map(([Key, Value]) => ({
			Key,
			Value,
		}));
	}
	return input;
}
export const lookupAttributes = [
	"EventId",
	"EventName",
	"EventSource",
	"Username",
	"ResourceType",
	"ResourceName",
	"ReadOnly",
	"AccessKeyId",
];
export type HistoryFilter = {
	attribute: string;
	value: string;
	start: string;
	end: string;
};
export function lookupInput(f: HistoryFilter): Record<string, unknown> {
	const input: Record<string, unknown> = { MaxResults: 50 };
	if (f.attribute) {
		if (!lookupAttributes.includes(f.attribute) || !f.value.trim())
			throw new Error("Choose a supported attribute and enter a value.");
		if (
			f.attribute === "ReadOnly" &&
			!["true", "false"].includes(f.value.trim())
		)
			throw new Error("ReadOnly must be true or false.");
		input.LookupAttributes = [
			{ AttributeKey: f.attribute, AttributeValue: f.value.trim() },
		];
	}
	for (const [key, value] of [
		["StartTime", f.start],
		["EndTime", f.end],
	]) {
		if (!value) continue;
		const time = new Date(value).getTime() / 1000;
		if (!Number.isFinite(time)) throw new Error("Enter a valid time range.");
		input[key] = time;
	}
	if (Number(input.StartTime) > Number(input.EndTime))
		throw new Error("Start time must be before end time.");
	return input;
}
export type TrailEvent = {
	EventId?: string;
	EventName?: string;
	EventSource?: string;
	Username?: string;
	ReadOnly?: string;
	AccessKeyId?: string;
	EventTime?: number;
	Resources?: { ResourceType?: string; ResourceName?: string }[];
	CloudTrailEvent?: string;
};
export function eventJSON(text?: string) {
	if (!text) return "-";
	try {
		return JSON.stringify(JSON.parse(text), null, 2);
	} catch {
		return text;
	}
}
function selectorObject(
	value: unknown,
	allowed: string[],
): Record<string, unknown> {
	if (
		!value ||
		typeof value !== "object" ||
		Array.isArray(value) ||
		Object.keys(value).some((key) => !allowed.includes(key))
	)
		throw new Error(`Selector fields must be: ${allowed.join(", ")}.`);
	return value as Record<string, unknown>;
}
function selectorList(value: unknown): unknown[] {
	if (!Array.isArray(value)) throw new Error("Selector lists must be arrays.");
	return value;
}
export function selectorInput(text: string): Record<string, unknown> {
	const value = selectorObject(JSON.parse(text), [
		"EventSelectors",
		"AdvancedEventSelectors",
	]);
	const basic =
		value.EventSelectors === undefined
			? []
			: selectorList(value.EventSelectors);
	const advanced =
		value.AdvancedEventSelectors === undefined
			? []
			: selectorList(value.AdvancedEventSelectors);
	if (basic.length > 0 === advanced.length > 0)
		throw new Error("Specify either basic or advanced selectors.");
	if (basic.length > 5 || advanced.length > 5)
		throw new Error("Use at most five selectors.");
	for (const item of basic) {
		const v = selectorObject(item, [
			"ReadWriteType",
			"IncludeManagementEvents",
			"DataResources",
			"ExcludeManagementEventSources",
		]);
		if (
			v.ReadWriteType !== undefined &&
			!["All", "ReadOnly", "WriteOnly"].includes(String(v.ReadWriteType))
		)
			throw new Error("Choose All, ReadOnly or WriteOnly.");
		if (v.DataResources !== undefined)
			for (const resource of selectorList(v.DataResources))
				selectorObject(resource, ["Type", "Values"]);
	}
	for (const item of advanced) {
		const v = selectorObject(item, ["Name", "FieldSelectors"]);
		const fields = selectorList(v.FieldSelectors);
		if (!fields.length)
			throw new Error("Advanced selectors require field selectors.");
		for (const field of fields) {
			const f = selectorObject(field, [
				"Field",
				"Equals",
				"StartsWith",
				"EndsWith",
				"NotEquals",
				"NotStartsWith",
				"NotEndsWith",
			]);
			if (typeof f.Field !== "string" || !f.Field)
				throw new Error("Field is required.");
			const operators = Object.entries(f).filter(([key]) => key !== "Field");
			if (!operators.some(([, values]) => selectorList(values).length > 0))
				throw new Error("Field selectors require values.");
			for (const [, values] of operators)
				if (selectorList(values).some((v) => typeof v !== "string"))
					throw new Error("Field selector values must be strings.");
		}
	}
	return value;
}
