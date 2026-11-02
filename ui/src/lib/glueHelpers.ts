export type GlueKind =
	| "Database"
	| "Table"
	| "Partition"
	| "Crawler"
	| "Job"
	| "Trigger"
	| "Workflow";
export type GlueRecord = Record<string, unknown> & {
	Name?: string;
	Values?: string[];
};
export type GlueContext = { DatabaseName?: string; TableName?: string };
export const glueKinds: GlueKind[] = [
	"Database",
	"Table",
	"Partition",
	"Crawler",
	"Job",
	"Trigger",
	"Workflow",
];
export const glueLists: Record<GlueKind, [string, string]> = {
	Database: ["GetDatabases", "DatabaseList"],
	Table: ["GetTables", "TableList"],
	Partition: ["GetPartitions", "Partitions"],
	Crawler: ["GetCrawlers", "Crawlers"],
	Job: ["GetJobs", "Jobs"],
	Trigger: ["GetTriggers", "Triggers"],
	Workflow: ["ListWorkflows", "Workflows"],
};
const shapes: Record<string, string[]> = {
	DatabaseInput: [
		"Name",
		"Description",
		"LocationUri",
		"Parameters",
		"CreateTableDefaultPermissions",
	],
	TableInput: [
		"Name",
		"Description",
		"Owner",
		"LastAccessTime",
		"Retention",
		"StorageDescriptor",
		"PartitionKeys",
		"ViewOriginalText",
		"ViewExpandedText",
		"TableType",
		"Parameters",
	],
	PartitionInput: [
		"Values",
		"LastAccessTime",
		"StorageDescriptor",
		"Parameters",
	],
	CreateJobRequest: [
		"Name",
		"Description",
		"Role",
		"ExecutionProperty",
		"Command",
		"DefaultArguments",
		"NonOverridableArguments",
		"MaxRetries",
		"Timeout",
		"MaxCapacity",
		"GlueVersion",
		"NumberOfWorkers",
		"WorkerType",
		"Tags",
	],
	JobUpdate: [
		"Description",
		"Role",
		"ExecutionProperty",
		"Command",
		"DefaultArguments",
		"NonOverridableArguments",
		"MaxRetries",
		"Timeout",
		"MaxCapacity",
		"GlueVersion",
		"NumberOfWorkers",
		"WorkerType",
	],
	CreateCrawlerRequest: [
		"Name",
		"Role",
		"DatabaseName",
		"Description",
		"Targets",
		"Classifiers",
		"TablePrefix",
		"Schedule",
		"SchemaChangePolicy",
		"RecrawlPolicy",
		"Configuration",
		"Tags",
	],
	UpdateCrawlerRequest: [
		"Name",
		"Role",
		"DatabaseName",
		"Description",
		"Targets",
		"Classifiers",
		"TablePrefix",
		"Schedule",
		"SchemaChangePolicy",
		"RecrawlPolicy",
		"Configuration",
	],
	CreateTriggerRequest: [
		"Name",
		"WorkflowName",
		"Type",
		"Schedule",
		"Predicate",
		"Actions",
		"Description",
		"StartOnCreation",
		"Tags",
	],
	TriggerUpdate: ["Name", "Description", "Schedule", "Actions", "Predicate"],
	CreateWorkflowRequest: [
		"Name",
		"Description",
		"DefaultRunProperties",
		"Tags",
		"MaxConcurrentRuns",
	],
	UpdateWorkflowRequest: [
		"Name",
		"Description",
		"DefaultRunProperties",
		"MaxConcurrentRuns",
	],
	StorageDescriptor: [
		"Columns",
		"Location",
		"InputFormat",
		"OutputFormat",
		"Compressed",
		"NumberOfBuckets",
		"SerdeInfo",
		"BucketColumns",
		"SortColumns",
		"Parameters",
		"SkewedInfo",
		"StoredAsSubDirectories",
	],
	Column: ["Name", "Type", "Comment", "Parameters"],
	SerDeInfo: ["Name", "SerializationLibrary", "Parameters"],
	Order: ["Column", "SortOrder"],
	SkewedInfo: [
		"SkewedColumnNames",
		"SkewedColumnValues",
		"SkewedColumnValueLocationMaps",
	],
	JobCommand: ["Name", "ScriptLocation", "PythonVersion", "Runtime"],
	ExecutionProperty: ["MaxConcurrentRuns"],
	CrawlerTargets: ["S3Targets"],
	S3Target: ["Path", "Exclusions", "SampleSize"],
	SchemaChangePolicy: ["UpdateBehavior", "DeleteBehavior"],
	RecrawlPolicy: ["RecrawlBehavior"],
	Predicate: ["Logical", "Conditions"],
	Condition: [
		"CrawlState",
		"CrawlerName",
		"LogicalOperator",
		"JobName",
		"State",
	],
	Action: ["CrawlerName", "JobName", "Arguments", "Timeout"],
	PrincipalPermissions: ["Principal", "Permissions"],
	DataLakePrincipal: ["DataLakePrincipalIdentifier"],
};
const children: Record<string, string> = {
	StorageDescriptor: "StorageDescriptor",
	Columns: "Column",
	PartitionKeys: "Column",
	SerdeInfo: "SerDeInfo",
	SortColumns: "Order",
	SkewedInfo: "SkewedInfo",
	Command: "JobCommand",
	ExecutionProperty: "ExecutionProperty",
	Targets: "CrawlerTargets",
	S3Targets: "S3Target",
	SchemaChangePolicy: "SchemaChangePolicy",
	RecrawlPolicy: "RecrawlPolicy",
	Predicate: "Predicate",
	Conditions: "Condition",
	Actions: "Action",
	CreateTableDefaultPermissions: "PrincipalPermissions",
	Principal: "DataLakePrincipal",
};
function validate(value: unknown, shape: string): void {
	if (!value || typeof value !== "object" || Array.isArray(value))
		throw new Error(`${shape} must be a JSON object.`);
	for (const [key, child] of Object.entries(value)) {
		if (!shapes[shape].includes(key))
			throw new Error(`${shape}.${key} is not supported.`);
		if (children[key]) {
			if (Array.isArray(child))
				for (const item of child) validate(item, children[key]);
			else validate(child, children[key]);
		}
	}
}
export function glueShape(kind: GlueKind, creating: boolean) {
	if (["Database", "Table", "Partition"].includes(kind)) return `${kind}Input`;
	if (creating) return `Create${kind}Request`;
	return kind === "Job"
		? "JobUpdate"
		: kind === "Trigger"
			? "TriggerUpdate"
			: `Update${kind}Request`;
}
export function glueName(row: GlueRecord) {
	return row.Name ?? JSON.stringify(row.Values ?? []);
}
export function glueIdentity(
	kind: GlueKind,
	row: GlueRecord,
	context: GlueContext = {},
): Record<string, unknown> {
	return {
		...context,
		...(kind === "Partition"
			? { PartitionValues: row.Values }
			: { [kind === "Job" ? "JobName" : "Name"]: row.Name }),
	};
}
export function glueDefinition(kind: GlueKind, row: GlueRecord): GlueRecord {
	const fields = shapes[glueShape(kind, false)];
	const out = Object.fromEntries(
		Object.entries(row).filter(([key]) => fields.includes(key)),
	);
	if (kind !== "Partition") out.Name = row.Name;
	if (kind === "Crawler" && row.Schedule && typeof row.Schedule === "object") {
		out.Schedule = (row.Schedule as Record<string, unknown>).ScheduleExpression;
	}
	return out;
}
export function glueInput(
	kind: GlueKind,
	text: string,
	creating: boolean,
	context: GlueContext = {},
	originalName?: string,
): Record<string, unknown> {
	let value: GlueRecord;
	try {
		value = JSON.parse(text);
	} catch {
		throw new Error("Configuration must be valid JSON.");
	}
	if (!value || typeof value !== "object" || Array.isArray(value))
		throw new Error("Configuration must be a JSON object.");
	const shape = glueShape(kind, creating);
	// JobUpdate has no Name member; the name is the immutable operation key.
	const body = { ...value };
	if (!creating && kind === "Job") delete body.Name;
	validate(body, shape);
	if (kind !== "Partition") {
		if (
			typeof value.Name !== "string" ||
			!value.Name.trim() ||
			new TextEncoder().encode(value.Name).length > 255
		)
			throw new Error("Name must contain 1 to 255 bytes.");
		if (["Database", "Table"].includes(kind))
			value.Name = value.Name.toLowerCase();
		if (!creating && value.Name !== originalName)
			throw new Error("Names cannot be changed.");
	}
	if (
		value.Tags !== undefined &&
		(!value.Tags ||
			typeof value.Tags !== "object" ||
			Array.isArray(value.Tags) ||
			Object.values(value.Tags).some((v) => typeof v !== "string"))
	)
		throw new Error("Tags must be an object of strings.");
	if (
		kind === "Partition" &&
		(!Array.isArray(value.Values) ||
			value.Values.some((v) => typeof v !== "string"))
	)
		throw new Error("Partition Values must be an array of strings.");
	if (kind === "Database")
		return {
			...context,
			...(!creating ? { Name: originalName } : {}),
			DatabaseInput: value,
		};
	if (kind === "Table") return { ...context, TableInput: value };
	if (kind === "Partition") return { ...context, PartitionInput: value };
	if (!creating && kind === "Job")
		return { JobName: originalName, JobUpdate: body };
	if (!creating && kind === "Trigger")
		return { Name: originalName, TriggerUpdate: value };
	return value;
}
export function emptyGlueDefinition(kind: GlueKind): GlueRecord {
	switch (kind) {
		case "Database":
			return { Name: "", Description: "", LocationUri: "", Parameters: {} };
		case "Table":
			return {
				Name: "",
				TableType: "EXTERNAL_TABLE",
				StorageDescriptor: {
					Columns: [],
					Location: "",
					InputFormat: "",
					OutputFormat: "",
					SerdeInfo: { SerializationLibrary: "", Parameters: {} },
				},
				PartitionKeys: [],
				Parameters: {},
			};
		case "Partition":
			return {
				Values: [],
				StorageDescriptor: { Location: "" },
				Parameters: {},
			};
		case "Crawler":
			return {
				Name: "",
				Role: "",
				DatabaseName: "",
				Targets: { S3Targets: [{ Path: "s3://" }] },
			};
		case "Job":
			return {
				Name: "",
				Role: "",
				Command: {
					Name: "pythonshell",
					ScriptLocation: "s3://",
					PythonVersion: "3.9",
				},
				DefaultArguments: {},
			};
		case "Trigger":
			return { Name: "", Type: "ON_DEMAND", Actions: [{ JobName: "" }] };
		case "Workflow":
			return { Name: "", Description: "", DefaultRunProperties: {} };
	}
}
