export type Pipe = {
	Name: string;
	Arn?: string;
	Description?: string;
	CurrentState?: string;
	DesiredState?: string;
	StateReason?: string;
	Source?: string;
	Target?: string;
	RoleArn?: string;
	Enrichment?: string;
	SourceParameters?: Record<string, unknown>;
	TargetParameters?: Record<string, unknown>;
	EnrichmentParameters?: Record<string, unknown>;
	CreationTime?: number;
	LastModifiedTime?: number;
	Tags?: Record<string, string>;
};
export type PipeForm = {
	name: string;
	description: string;
	source: string;
	target: string;
	role: string;
	enrichment: string;
	desiredState: string;
	sourceParameters: string;
	targetParameters: string;
	enrichmentParameters: string;
	tags: string;
};
export function emptyPipeForm(): PipeForm {
	return {
		name: "",
		description: "",
		source: "",
		target: "",
		role: "",
		enrichment: "",
		desiredState: "STOPPED",
		sourceParameters: "{}",
		targetParameters: "{}",
		enrichmentParameters: "{}",
		tags: "{}",
	};
}
function object(text: string, label: string): Record<string, unknown> {
	let value: unknown;
	try {
		value = JSON.parse(text || "{}");
	} catch {
		throw new Error(`${label} must be valid JSON.`);
	}
	if (!value || typeof value !== "object" || Array.isArray(value))
		throw new Error(`${label} must be a JSON object.`);
	return value as Record<string, unknown>;
}
function members(
	value: Record<string, unknown>,
	allowed: string[],
	label: string,
) {
	for (const key of Object.keys(value))
		if (!allowed.includes(key))
			throw new Error(`${label}.${key} is not supported.`);
}
function block(value: Record<string, unknown>, key: string, allowed: string[]) {
	if (value[key] === undefined) return undefined;
	const out = object(JSON.stringify(value[key]), key);
	members(out, allowed, key);
	return out;
}
export function pipeInput(
	f: PipeForm,
	creating = true,
): Record<string, unknown> {
	const name = f.name.trim();
	if (!/^[.\-_A-Za-z0-9]{1,64}$/.test(name))
		throw new Error(
			"Name must be 1 to 64 letters, digits, periods, hyphens or underscores.",
		);
	if (f.description.length > 512)
		throw new Error("Description must be at most 512 characters.");
	if (!["RUNNING", "STOPPED"].includes(f.desiredState))
		throw new Error("Choose RUNNING or STOPPED.");
	if (!f.role.trim() || !f.target.trim() || !f.source.trim())
		throw new Error("Source, target and role ARNs are required.");
	const source = object(f.sourceParameters, "Source parameters");
	members(
		source,
		[
			"FilterCriteria",
			"SqsQueueParameters",
			"KinesisStreamParameters",
			"DynamoDBStreamParameters",
		],
		"Source parameters",
	);
	for (const key of [
		"SqsQueueParameters",
		"KinesisStreamParameters",
		"DynamoDBStreamParameters",
	]) {
		const b = block(source, key, [
			"BatchSize",
			"MaximumBatchingWindowInSeconds",
			...(creating && key !== "SqsQueueParameters" ? ["StartingPosition"] : []),
		]);
		if (
			b?.MaximumBatchingWindowInSeconds !== undefined &&
			b.MaximumBatchingWindowInSeconds !== 0
		)
			throw new Error("Batching windows must be zero.");
		if (
			b?.StartingPosition !== undefined &&
			!["LATEST", "TRIM_HORIZON"].includes(String(b.StartingPosition))
		)
			throw new Error("StartingPosition must be LATEST or TRIM_HORIZON.");
	}
	const filters = block(source, "FilterCriteria", ["Filters"]);
	if (filters?.Filters !== undefined) {
		if (!Array.isArray(filters.Filters) || filters.Filters.length > 5)
			throw new Error("Filters must be an array of at most five patterns.");
		for (const filter of filters.Filters) {
			const item = object(JSON.stringify(filter), "Filter");
			members(item, ["Pattern"], "Filter");
			if (typeof item.Pattern !== "string")
				throw new Error("Each filter needs a Pattern string.");
			object(item.Pattern, "Filter pattern");
		}
	}
	const target = object(f.targetParameters, "Target parameters");
	members(
		target,
		[
			"InputTemplate",
			"LambdaFunctionParameters",
			"StepFunctionStateMachineParameters",
			"KinesisStreamParameters",
			"SqsQueueParameters",
			"EventBridgeEventBusParameters",
		],
		"Target parameters",
	);
	block(target, "LambdaFunctionParameters", ["InvocationType"]);
	block(target, "StepFunctionStateMachineParameters", ["InvocationType"]);
	block(target, "KinesisStreamParameters", ["PartitionKey"]);
	block(target, "SqsQueueParameters", [
		"MessageGroupId",
		"MessageDeduplicationId",
	]);
	block(target, "EventBridgeEventBusParameters", ["DetailType", "Source"]);
	const enrichment = object(f.enrichmentParameters, "Enrichment parameters");
	members(enrichment, ["InputTemplate"], "Enrichment parameters");
	const input: Record<string, unknown> = {
		Name: name,
		Description: f.description,
		DesiredState: f.desiredState,
		RoleArn: f.role.trim(),
		Target: f.target.trim(),
		Enrichment: f.enrichment.trim(),
		SourceParameters: source,
		TargetParameters: target,
		EnrichmentParameters: enrichment,
	};
	if (creating) {
		input.Source = f.source.trim();
		const tags = object(f.tags, "Tags");
		if (Object.values(tags).some((v) => typeof v !== "string"))
			throw new Error("Tags must have string values.");
		input.Tags = tags;
	}
	return input;
}
export function formFromPipe(p: Pipe): PipeForm {
	const source = structuredClone(p.SourceParameters ?? {});
	// UpdatePipe cannot change stream starting positions.
	for (const key of ["KinesisStreamParameters", "DynamoDBStreamParameters"]) {
		const b = source[key] as Record<string, unknown> | undefined;
		if (b) {
			delete b.StartingPosition;
			delete b.StartingPositionTimestamp;
		}
	}
	return {
		name: p.Name,
		description: p.Description ?? "",
		source: p.Source ?? "",
		target: p.Target ?? "",
		role: p.RoleArn ?? "",
		enrichment: p.Enrichment ?? "",
		desiredState: p.DesiredState ?? "STOPPED",
		sourceParameters: JSON.stringify(source, null, 2),
		targetParameters: JSON.stringify(p.TargetParameters ?? {}, null, 2),
		enrichmentParameters: JSON.stringify(p.EnrichmentParameters ?? {}, null, 2),
		tags: JSON.stringify(p.Tags ?? {}, null, 2),
	};
}
