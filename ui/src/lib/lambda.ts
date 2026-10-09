import { api, type Resource } from "./api";
import { integer, stringMap } from "./consoleActions";

export type LambdaScope = {
	account: string;
	region: string;
	functionName?: string;
};
export type FunctionConfiguration = {
	FunctionName?: string;
	FunctionArn?: string;
	Runtime?: string;
	Handler?: string;
	Role?: string;
	Description?: string;
	Timeout?: number;
	MemorySize?: number;
	CodeSize?: number;
	CodeSha256?: string;
	LastModified?: string;
	RevisionId?: string;
	PackageType?: string;
	Architectures?: string[];
	State?: string;
	LastUpdateStatus?: string;
	Version?: string;
	Environment?: { Variables?: Record<string, string> };
	Layers?: { Arn: string; CodeSize?: number }[];
};
export type LambdaFunction = {
	Configuration?: FunctionConfiguration;
	Code?: { RepositoryType?: string; ImageUri?: string };
	Tags?: Record<string, string>;
};
export type LambdaMapping = {
	UUID: string;
	FunctionArn: string;
	EventSourceArn: string;
	State: string;
	BatchSize: number;
	MaximumBatchingWindowInSeconds?: number;
	FunctionResponseTypes?: string[];
	LastProcessingResult?: string;
	FilterCriteria?: { Filters?: { Pattern: string }[] };
};
export const runtimes = [
	"python3.14",
	"python3.13",
	"python3.12",
	"python3.11",
	"python3.10",
	"python3.9",
	"nodejs24.x",
	"nodejs22.x",
	"nodejs20.x",
	"nodejs18.x",
	"java25",
	"java21",
	"java17",
	"java11",
	"ruby3.4",
	"ruby3.3",
	"ruby3.2",
	"dotnet10",
	"dotnet8",
	"provided.al2023",
	"provided.al2",
];
export function lambdaHref(scope: LambdaScope, create = false) {
	const params = new URLSearchParams({
		account: scope.account,
		region: scope.region,
	});
	if (scope.functionName) params.set("function", scope.functionName);
	if (create) params.set("create", "true");
	return `/services/lambda?${params}`;
}
export function functionArn(scope: LambdaScope) {
	return `arn:aws:lambda:${scope.region}:${scope.account}:function:${scope.functionName}`;
}
export function lambdaCall<T>(
	scope: LambdaScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("lambda", operation, scope.account, input, scope.region);
}
export function functionCall<T>(
	scope: LambdaScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return lambdaCall(scope, operation, {
		FunctionName: scope.functionName,
		...input,
	});
}
export function functionConfig(resource?: Resource): FunctionConfiguration {
	return (resource?.details as FunctionConfiguration) ?? {};
}
export async function functionMappings(
	scope: LambdaScope,
): Promise<LambdaMapping[]> {
	const mappings: LambdaMapping[] = [];
	const seen = new Set<string>();
	let marker: string | undefined;
	do {
		const page = await functionCall<{
			EventSourceMappings?: LambdaMapping[];
			NextMarker?: string;
		}>(scope, "ListEventSourceMappings", {
			MaxItems: 50,
			...(marker ? { Marker: marker } : {}),
		});
		mappings.push(...(page.EventSourceMappings ?? []));
		marker = page.NextMarker;
		if (marker && seen.has(marker))
			throw new Error(
				"Event source mapping pagination did not advance. Refresh and try again.",
			);
		if (marker) seen.add(marker);
	} while (marker);
	return mappings;
}
export function encodeBytes(bytes: Uint8Array): string {
	let binary = "";
	for (let i = 0; i < bytes.length; i += 8192)
		binary += String.fromCharCode(...bytes.subarray(i, i + 8192));
	return btoa(binary);
}
export function decodeText(base64: string): string {
	return new TextDecoder().decode(
		Uint8Array.from(atob(base64), (c) => c.charCodeAt(0)),
	);
}
export type CodeInput = {
	source: "zip" | "s3" | "image";
	files: File[];
	bucket: string;
	key: string;
	version: string;
	image: string;
};
export function codeDefaults(image = ""): CodeInput {
	return {
		source: image ? "image" : "zip",
		files: [],
		bucket: "",
		key: "",
		version: "",
		image,
	};
}
export async function deploymentCode(
	code: CodeInput,
): Promise<Record<string, unknown>> {
	if (code.source === "image") {
		if (!code.image.trim()) throw new Error("Enter a container image URI.");
		return { ImageUri: code.image.trim() };
	}
	if (code.source === "s3") {
		if (!code.bucket.trim() || !code.key)
			throw new Error("Enter the S3 bucket and object key.");
		return {
			S3Bucket: code.bucket.trim(),
			S3Key: code.key,
			...(code.version ? { S3ObjectVersion: code.version } : {}),
		};
	}
	const file = code.files[0];
	if (!file?.name.toLowerCase().endsWith(".zip"))
		throw new Error("Select a ZIP deployment package.");
	if (file.size > 1024 * 1024)
		throw new Error(
			"Direct ZIP uploads are limited to 1 MiB. Deploy larger packages from S3.",
		);
	return { ZipFile: encodeBytes(new Uint8Array(await file.arrayBuffer())) };
}
export type SettingsInput = {
	role: string;
	description: string;
	runtime: string;
	handler: string;
	memory: string;
	timeout: string;
	environment: string;
};
export function settingsDefaults(
	config: FunctionConfiguration = {},
): SettingsInput {
	return {
		role: config.Role ?? "",
		description: config.Description ?? "",
		runtime: config.Runtime ?? "python3.12",
		handler: config.Handler ?? "lambda_function.lambda_handler",
		memory: String(config.MemorySize ?? 128),
		timeout: String(config.Timeout ?? 3),
		environment: JSON.stringify(config.Environment?.Variables ?? {}, null, 2),
	};
}
export function configurationInput(
	settings: SettingsInput,
	image: boolean,
): Record<string, unknown> {
	if (
		!/^arn:aws[a-zA-Z-]*:iam::\d{12}:role\/[a-zA-Z0-9+=,.@_/-]+$/.test(
			settings.role,
		)
	)
		throw new Error("Enter an IAM execution role ARN.");
	if (settings.description.length > 256)
		throw new Error("Description must be at most 256 characters.");
	if (!image && (!settings.handler || /\s/.test(settings.handler)))
		throw new Error("Enter a handler without whitespace.");
	const variables = stringMap(settings.environment, "Environment variables");
	return {
		Role: settings.role,
		Description: settings.description,
		MemorySize: integer(settings.memory, "Memory", 128, 10240),
		Timeout: integer(settings.timeout, "Timeout", 1, 900),
		Environment: { Variables: variables },
		...(!image ? { Runtime: settings.runtime, Handler: settings.handler } : {}),
	};
}
