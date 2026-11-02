export type ResultConfiguration = {
	OutputLocation?: string;
	EncryptionConfiguration?: { EncryptionOption?: string; KmsKey?: string };
	ExpectedBucketOwner?: string;
};
export type WorkGroup = {
	Name: string;
	State?: string;
	Description?: string;
	CreationTime?: number;
	Configuration?: {
		ResultConfiguration?: ResultConfiguration;
		EnforceWorkGroupConfiguration?: boolean;
		PublishCloudWatchMetricsEnabled?: boolean;
		RequesterPaysEnabled?: boolean;
		BytesScannedCutoffPerQuery?: number;
		EngineVersion?: {
			SelectedEngineVersion?: string;
			EffectiveEngineVersion?: string;
		};
	};
};
export type QueryExecution = {
	QueryExecutionId: string;
	Query?: string;
	WorkGroup?: string;
	StatementType?: string;
	ExecutionParameters?: string[];
	QueryExecutionContext?: { Catalog?: string; Database?: string };
	ResultConfiguration?: ResultConfiguration;
	Status?: {
		State?: string;
		StateChangeReason?: string;
		SubmissionDateTime?: number;
		CompletionDateTime?: number;
	};
	Statistics?: {
		EngineExecutionTimeInMillis?: number;
		TotalExecutionTimeInMillis?: number;
		DataScannedInBytes?: number;
		QueryQueueTimeInMillis?: number;
		QueryPlanningTimeInMillis?: number;
		ServiceProcessingTimeInMillis?: number;
	};
};
export type NamedQuery = {
	NamedQueryId: string;
	Name: string;
	Description?: string;
	QueryString: string;
	Database: string;
	WorkGroup?: string;
};
export type Results = {
	NextToken?: string;
	UpdateCount?: number;
	ResultSet?: {
		Rows?: { Data?: { VarCharValue?: string }[] }[];
		ResultSetMetadata?: {
			ColumnInfo?: { Name?: string; Label?: string; Type?: string }[];
		};
	};
};
// Completed executions point at a result file; re-runs need its original prefix.
export function rerunOutput(query?: QueryExecution) {
	const output = query?.ResultConfiguration?.OutputLocation ?? "";
	for (const extension of ["csv", "txt"]) {
		const suffix = `/${query?.QueryExecutionId}.${extension}`;
		if (output.endsWith(suffix)) return `${output.slice(0, -suffix.length)}/`;
	}
	return output;
}
export function terminal(state?: string) {
	return ["SUCCEEDED", "FAILED", "CANCELLED"].includes(state ?? "");
}
export function queryInput(
	sql: string,
	workgroup: string,
	catalog: string,
	database: string,
	output: string,
	parameters: string[] = [],
) {
	if (!sql.trim()) throw new Error("SQL is required.");
	if (new TextEncoder().encode(sql).length > 262144)
		throw new Error("SQL must be at most 262144 bytes.");
	return {
		QueryString: sql,
		WorkGroup: workgroup,
		QueryExecutionContext: {
			Catalog: catalog,
			...(database ? { Database: database } : {}),
		},
		...(output.trim()
			? { ResultConfiguration: { OutputLocation: output.trim() } }
			: {}),
		...(parameters.length ? { ExecutionParameters: parameters } : {}),
	};
}
export function namedQueryInput(
	name: string,
	description: string,
	sql: string,
	database: string,
	workgroup: string,
) {
	if (!name.trim() || new TextEncoder().encode(name.trim()).length > 128)
		throw new Error("Name must be 1 to 128 bytes.");
	if (!database) throw new Error("A database is required to save a query.");
	if (!sql.trim()) throw new Error("SQL is required.");
	return {
		Name: name.trim(),
		Description: description,
		QueryString: sql,
		Database: database,
		WorkGroup: workgroup,
	};
}
export type WorkgroupSettings = {
	encryption: string;
	kmsKey: string;
	owner: string;
	cutoff: string;
	metrics: boolean;
	requesterPays: boolean;
	engine: string;
};
export function workgroupSettings(group?: WorkGroup): WorkgroupSettings {
	const c = group?.Configuration,
		r = c?.ResultConfiguration;
	return {
		encryption: r?.EncryptionConfiguration?.EncryptionOption ?? "None",
		kmsKey: r?.EncryptionConfiguration?.KmsKey ?? "",
		owner: r?.ExpectedBucketOwner ?? "",
		cutoff:
			c?.BytesScannedCutoffPerQuery === undefined
				? ""
				: String(c.BytesScannedCutoffPerQuery),
		metrics: c?.PublishCloudWatchMetricsEnabled ?? true,
		requesterPays: c?.RequesterPaysEnabled ?? false,
		engine: c?.EngineVersion?.SelectedEngineVersion ?? "AUTO",
	};
}
export function workgroupInput(
	name: string,
	description: string,
	output: string,
	enforce: boolean,
	editing: boolean,
	settings: WorkgroupSettings,
) {
	if (!/^[a-zA-Z0-9._-]{1,128}$/.test(name))
		throw new Error(
			"Workgroup name must be 1 to 128 letters, digits, periods, underscores or dashes.",
		);
	if (output && !/^s3:\/\/[^/]+/.test(output))
		throw new Error("Output location must be an s3:// URI.");
	const result: Record<string, unknown> = output
		? { OutputLocation: output }
		: editing
			? { RemoveOutputLocation: true }
			: {};
	const configuration: NonNullable<WorkGroup["Configuration"]> & {
		RemoveBytesScannedCutoffPerQuery?: boolean;
	} = {};
	if (settings.owner && !/^\d{12}$/.test(settings.owner))
		throw new Error("Expected bucket owner must be a 12-digit account ID.");
	if (
		settings.cutoff &&
		(!/^\d+$/.test(settings.cutoff) ||
			!Number.isSafeInteger(Number(settings.cutoff)))
	)
		throw new Error(
			"Bytes scanned cutoff must be a non-negative whole number.",
		);
	if (settings.encryption !== "None") {
		if (settings.encryption !== "SSE_S3" && !settings.kmsKey.trim())
			throw new Error("A KMS key is required for KMS encryption.");
		result.EncryptionConfiguration = {
			EncryptionOption: settings.encryption,
			...(settings.encryption !== "SSE_S3"
				? { KmsKey: settings.kmsKey.trim() }
				: {}),
		};
	} else if (editing) result.RemoveEncryptionConfiguration = true;
	if (settings.owner) result.ExpectedBucketOwner = settings.owner;
	else if (editing) result.RemoveExpectedBucketOwner = true;
	configuration.PublishCloudWatchMetricsEnabled = settings.metrics;
	configuration.RequesterPaysEnabled = settings.requesterPays;
	configuration.EngineVersion = { SelectedEngineVersion: settings.engine };
	if (settings.cutoff)
		configuration.BytesScannedCutoffPerQuery = Number(settings.cutoff);
	else if (editing) configuration.RemoveBytesScannedCutoffPerQuery = true;

	return editing
		? {
				WorkGroup: name,
				Description: description,
				ConfigurationUpdates: {
					...configuration,
					EnforceWorkGroupConfiguration: enforce,
					ResultConfigurationUpdates: result,
				},
			}
		: {
				Name: name,
				Description: description,
				Configuration: {
					...configuration,
					EnforceWorkGroupConfiguration: enforce,
					ResultConfiguration: result,
				},
			};
}
// Only the first DML result page includes the header. DDL/UTILITY rows are data.
export function resultRows(
	result: Results,
	firstPage: boolean,
	statementType?: string,
) {
	const rows = result.ResultSet?.Rows ?? [];
	return (firstPage && statementType === "DML" ? rows.slice(1) : rows).map(
		(row) => row.Data?.map((cell) => cell.VarCharValue ?? null) ?? [],
	);
}
export function batches<T>(items: T[], size = 50): T[][] {
	if (!Number.isSafeInteger(size) || size < 1)
		throw new Error("Invalid batch size.");
	const out: T[][] = [];
	for (let i = 0; i < items.length; i += size)
		out.push(items.slice(i, i + size));
	return out;
}
