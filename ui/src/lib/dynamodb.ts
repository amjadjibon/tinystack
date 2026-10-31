import { api, type Resource } from "./api";
import { integer, jsonObject } from "./consoleActions";

export type DynamoScope = { account: string; region: string; table?: string };
export type Attribute = {
	S?: string;
	N?: string;
	B?: string;
	BOOL?: boolean;
	NULL?: boolean;
	SS?: string[];
	NS?: string[];
	BS?: string[];
	L?: Attribute[];
	M?: Item;
};
export type Item = Record<string, Attribute>;
export type KeySchema = { AttributeName: string; KeyType: "HASH" | "RANGE" }[];
export type Capacity = {
	ReadCapacityUnits?: number;
	WriteCapacityUnits?: number;
};
export type DynamoIndex = {
	IndexName: string;
	KeySchema: KeySchema;
	Projection?: { ProjectionType: string; NonKeyAttributes?: string[] };
	IndexStatus?: string;
	ItemCount?: number;
	IndexSizeBytes?: number;
	ProvisionedThroughput?: Capacity;
};
export type DynamoTable = {
	TableName?: string;
	TableArn?: string;
	TableStatus?: string;
	KeySchema?: KeySchema;
	AttributeDefinitions?: { AttributeName: string; AttributeType: string }[];
	ItemCount?: number;
	TableSizeBytes?: number;
	CreationDateTime?: number;
	BillingModeSummary?: { BillingMode: string };
	ProvisionedThroughput?: Capacity;
	GlobalSecondaryIndexes?: DynamoIndex[];
	LocalSecondaryIndexes?: DynamoIndex[];
	StreamSpecification?: { StreamEnabled: boolean; StreamViewType?: string };
	LatestStreamArn?: string;
};
export type ReadPage = {
	Items?: Item[];
	Count?: number;
	ScannedCount?: number;
	LastEvaluatedKey?: Item;
};
export const streamViews = [
	"KEYS_ONLY",
	"NEW_IMAGE",
	"OLD_IMAGE",
	"NEW_AND_OLD_IMAGES",
];
export function dynamoHref(
	scope: DynamoScope,
	create = false,
	section = "tables",
) {
	const params = new URLSearchParams({
		account: scope.account,
		region: scope.region,
	});
	if (scope.table) params.set("table", scope.table);
	if (create) params.set("create", "true");
	if (section === "items") params.set("section", section);
	return `/services/dynamodb?${params}`;
}
export function dynamoCall<T = Record<string, unknown>>(
	scope: DynamoScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("dynamodb", operation, scope.account, input, scope.region);
}
export function tableCall<T = Record<string, unknown>>(
	scope: DynamoScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return dynamoCall(scope, operation, { TableName: scope.table, ...input });
}
export function tableArn(scope: DynamoScope) {
	return `arn:aws:dynamodb:${scope.region}:${scope.account}:table/${scope.table}`;
}
export function tableDetails(resource?: Resource): DynamoTable {
	return (resource?.details as DynamoTable) ?? {};
}
export function dynamoTags(tags: Record<string, string>) {
	return Object.entries(tags).map(([Key, Value]) => ({ Key, Value }));
}
export function keySchema(partition: string, sort = ""): KeySchema {
	if (
		!partition ||
		new TextEncoder().encode(partition).length > 255 ||
		new TextEncoder().encode(sort).length > 255 ||
		partition === sort
	)
		throw new Error(
			"Enter a partition key and an optional distinct sort key, up to 255 bytes each.",
		);
	return [
		{ AttributeName: partition, KeyType: "HASH" },
		...(sort ? [{ AttributeName: sort, KeyType: "RANGE" as const }] : []),
	];
}
export function attributeDefinitions(
	keys: { AttributeName: string; AttributeType: string }[],
) {
	const types = new Map<string, string>();
	for (const key of keys) {
		if (
			types.has(key.AttributeName) &&
			types.get(key.AttributeName) !== key.AttributeType
		)
			throw new Error(
				`Attribute ${key.AttributeName} must use the same type in every key schema.`,
			);
		types.set(key.AttributeName, key.AttributeType);
	}
	return [...types].map(([AttributeName, AttributeType]) => ({
		AttributeName,
		AttributeType,
	}));
}
export type IndexInput = {
	name: string;
	partition: string;
	partitionType: string;
	sort: string;
	sortType: string;
	projection: string;
	nonKeys: string;
	read: string;
	write: string;
};
export const indexDefaults = (): IndexInput => ({
	name: "",
	partition: "",
	partitionType: "S",
	sort: "",
	sortType: "S",
	projection: "ALL",
	nonKeys: "",
	read: "5",
	write: "5",
});
export function buildIndex(index: IndexInput, provisioned: boolean) {
	if (!/^[A-Za-z0-9_.-]{3,255}$/.test(index.name))
		throw new Error(
			"Index names use 3–255 letters, numbers, underscores, hyphens or periods.",
		);
	const schema = keySchema(index.partition, index.sort);
	const nonKeys = index.nonKeys
		.split(",")
		.map((v) => v.trim())
		.filter(Boolean);
	if (index.projection === "INCLUDE" && !nonKeys.length)
		throw new Error("Enter non-key attributes for an INCLUDE projection.");
	return {
		IndexName: index.name,
		KeySchema: schema,
		Projection: {
			ProjectionType: index.projection,
			...(index.projection === "INCLUDE" ? { NonKeyAttributes: nonKeys } : {}),
		},
		...(provisioned
			? { ProvisionedThroughput: capacity(index.read, index.write) }
			: {}),
	};
}
export function indexDefinitions(index: IndexInput) {
	return [
		{ AttributeName: index.partition, AttributeType: index.partitionType },
		...(index.sort
			? [{ AttributeName: index.sort, AttributeType: index.sortType }]
			: []),
	];
}
export function capacity(read: string, write: string) {
	return {
		ReadCapacityUnits: integer(read, "Read capacity", 1, 1000000000),
		WriteCapacityUnits: integer(write, "Write capacity", 1, 1000000000),
	};
}
export function typedItem(text: string, label = "Item"): Item {
	const value = jsonObject(text, label);
	const validate = (attribute: unknown, path: string, depth: number) => {
		if (depth > 32) throw new Error(`${path} exceeds 32 nesting levels.`);
		if (!attribute || typeof attribute !== "object" || Array.isArray(attribute))
			throw new Error(`${path} must be a DynamoDB type object.`);
		const entries = Object.entries(attribute);
		if (entries.length !== 1)
			throw new Error(`${path} must contain exactly one DynamoDB type.`);
		const [type, v] = entries[0];
		if (["S", "N", "B"].includes(type)) {
			if (typeof v !== "string")
				throw new Error(`${path}.${type} must be a string.`);
			if (type === "B") {
				try {
					atob(v);
				} catch {
					throw new Error(`${path}.B must be base64.`);
				}
			}
		} else if (type === "BOOL" || type === "NULL") {
			if (typeof v !== "boolean" || (type === "NULL" && !v))
				throw new Error(
					`${path}.${type} must be ${type === "NULL" ? "true" : "a boolean"}.`,
				);
		} else if (["SS", "NS", "BS"].includes(type)) {
			if (
				!Array.isArray(v) ||
				!v.length ||
				v.some((x) => typeof x !== "string")
			)
				throw new Error(`${path}.${type} must be a nonempty array of strings.`);
			if (type === "BS")
				for (const x of v) {
					try {
						atob(x);
					} catch {
						throw new Error(`${path}.BS must contain base64 strings.`);
					}
				}
		} else if (type === "L") {
			if (!Array.isArray(v)) throw new Error(`${path}.L must be an array.`);
			for (const [i, x] of v.entries()) validate(x, `${path}[${i}]`, depth + 1);
		} else if (type === "M") {
			if (!v || typeof v !== "object" || Array.isArray(v))
				throw new Error(`${path}.M must be an object.`);
			for (const [k, x] of Object.entries(v))
				validate(x, `${path}.${k}`, depth + 1);
		} else throw new Error(`${path} uses unknown DynamoDB type ${type}.`);
	};
	for (const [name, attribute] of Object.entries(value))
		validate(attribute, `${label}.${name}`, 0);
	return value as Item;
}
export function itemKey(item: Item, schema: KeySchema): Item {
	for (const { AttributeName } of schema) {
		if (!Object.hasOwn(item, AttributeName))
			throw new Error(`Missing key attribute ${AttributeName}.`);
	}
	return Object.fromEntries(
		schema.map(({ AttributeName }) => [AttributeName, item[AttributeName]]),
	);
}
export function newItem(table: DynamoTable): Item {
	return Object.fromEntries(
		(table.KeySchema ?? []).map((k) => {
			const type =
				table.AttributeDefinitions?.find(
					(d) => d.AttributeName === k.AttributeName,
				)?.AttributeType ?? "S";
			return [
				k.AttributeName,
				{ [type]: type === "N" ? "0" : type === "B" ? "a2V5" : "value" },
			];
		}),
	);
}
export function editItemInput(before: Item, after: Item, schema: KeySchema) {
	for (const k of schema)
		if (
			JSON.stringify(before[k.AttributeName]) !==
			JSON.stringify(after[k.AttributeName])
		)
			throw new Error(
				"Primary key values cannot be changed. Create a new item to use a different key.",
			);
	const names: Record<string, string> = { "#pk": schema[0].AttributeName };
	const values: Item = {};
	const set: string[] = [],
		remove: string[] = [],
		conditions = ["attribute_exists(#pk)"];
	for (const name of new Set([...Object.keys(before), ...Object.keys(after)])) {
		const old = Object.hasOwn(before, name) ? before[name] : undefined;
		const next = Object.hasOwn(after, name) ? after[name] : undefined;
		if (
			schema.some((k) => k.AttributeName === name) ||
			JSON.stringify(old) === JSON.stringify(next)
		)
			continue;
		const alias = `#a${Object.keys(names).length}`;
		names[alias] = name;
		if (old) {
			const token = `:old${Object.keys(values).length}`;
			values[token] = old;
			conditions.push(`${alias} = ${token}`);
		} else conditions.push(`attribute_not_exists(${alias})`);
		if (next) {
			const token = `:new${Object.keys(values).length}`;
			values[token] = next;
			set.push(`${alias} = ${token}`);
		} else remove.push(alias);
	}
	if (!set.length && !remove.length)
		throw new Error("No item changes to save.");
	return {
		Key: itemKey(before, schema),
		UpdateExpression: [
			set.length ? `SET ${set.join(", ")}` : "",
			remove.length ? `REMOVE ${remove.join(", ")}` : "",
		]
			.filter(Boolean)
			.join(" "),
		ConditionExpression: conditions.join(" AND "),
		ExpressionAttributeNames: names,
		...(Object.keys(values).length
			? { ExpressionAttributeValues: values }
			: {}),
	};
}

export type BackupSummary = {
	BackupArn: string;
	BackupName: string;
	BackupStatus: string;
};
export type ExportSummary = { ExportArn: string; ExportStatus: string };
export type KinesisDestination = {
	StreamArn: string;
	DestinationStatus: string;
	DestinationStatusDescription?: string;
	ApproximateCreationDateTimePrecision: string;
};
export type ContinuousBackups = {
	ContinuousBackupsDescription?: {
		PointInTimeRecoveryDescription?: {
			PointInTimeRecoveryStatus?: string;
			EarliestRestorableDateTime?: number;
			LatestRestorableDateTime?: number;
		};
	};
};
// Collect service pages so management lists never hide resources after page one.
export async function listBackups(scope: DynamoScope) {
	const rows: BackupSummary[] = [];
	let cursor: string | undefined;
	do {
		const page = await tableCall<{
			BackupSummaries?: BackupSummary[];
			LastEvaluatedBackupArn?: string;
		}>(scope, "ListBackups", cursor ? { ExclusiveStartBackupArn: cursor } : {});
		rows.push(...(page.BackupSummaries ?? []));
		cursor = page.LastEvaluatedBackupArn;
	} while (cursor);
	return rows;
}
export async function listExports(scope: DynamoScope) {
	const rows: ExportSummary[] = [];
	let cursor: string | undefined;
	do {
		const page = await dynamoCall<{
			ExportSummaries?: ExportSummary[];
			NextToken?: string;
		}>(scope, "ListExports", {
			TableArn: tableArn(scope),
			...(cursor ? { NextToken: cursor } : {}),
		});
		rows.push(...(page.ExportSummaries ?? []));
		cursor = page.NextToken;
	} while (cursor);
	return rows;
}
