// Pure AppSync form and wire helpers. Field names follow console action SDK shapes.
export type RecordValue = Record<string, unknown>;
export type GraphqlApi = RecordValue & {
	ApiId: string;
	Name?: string;
	Arn?: string;
	AuthenticationType?: string;
	Uris?: Record<string, string>;
	Tags?: Record<string, string>;
};
export type ResourceKind = "api" | "source" | "resolver" | "function" | "key";
export const authTypes = [
	"API_KEY",
	"AWS_IAM",
	"AMAZON_COGNITO_USER_POOLS",
	"OPENID_CONNECT",
	"AWS_LAMBDA",
];
export const sourceTypes = [
	"NONE",
	"AWS_LAMBDA",
	"AMAZON_DYNAMODB",
	"HTTP",
	"AMAZON_EVENTBRIDGE",
];
export type FieldSpec = {
	key: string;
	label: string;
	kind?: "json" | "boolean" | "number" | "text";
	choices?: string[];
	required?: boolean;
	schema?: string;
	example?: string;
};
const field = (
	key: string,
	label: string,
	extra: Omit<FieldSpec, "key" | "label"> = {},
): FieldSpec => ({ key, label, ...extra });
const executionFields: FieldSpec[] = [
	field("DataSourceName", "Data source name"),
	field("RequestMappingTemplate", "Request mapping template", { kind: "text" }),
	field("ResponseMappingTemplate", "Response mapping template", {
		kind: "text",
	}),
	field("Runtime", "Runtime (JSON)", {
		kind: "json",
		schema: "AppSyncRuntime",
		example: '{"Name":"APPSYNC_JS","RuntimeVersion":"1.0.0"}',
	}),
	field("Code", "JavaScript code", { kind: "text" }),
	field("MaxBatchSize", "Maximum batch size", { kind: "number" }),
];
export const fields: Record<ResourceKind, FieldSpec[]> = {
	api: [
		field("Name", "Name", { required: true }),
		field("AuthenticationType", "Authentication type", {
			choices: authTypes,
			required: true,
		}),
		field(
			"AdditionalAuthenticationProviders",
			"Additional providers (JSON array)",
			{ kind: "json", schema: "AdditionalAuthenticationProvider" },
		),
		field("UserPoolConfig", "Cognito configuration (JSON)", {
			kind: "json",
			schema: "UserPoolConfig",
			example:
				'{"UserPoolId":"pool-id","AwsRegion":"us-east-1","DefaultAction":"ALLOW"}',
		}),
		field("OpenIDConnectConfig", "OIDC configuration (JSON)", {
			kind: "json",
			schema: "OpenIDConnectConfig",
			example: '{"Issuer":"https://issuer.example","ClientId":"client"}',
		}),
		field("LambdaAuthorizerConfig", "Lambda authorizer configuration (JSON)", {
			kind: "json",
			schema: "LambdaAuthorizerConfig",
			example:
				'{"AuthorizerUri":"arn:aws:lambda:region:account:function:authorizer","AuthorizerResultTtlInSeconds":0}',
		}),
		field("LogConfig", "Log configuration (JSON)", {
			kind: "json",
			schema: "LogConfig",
			example:
				'{"FieldLogLevel":"ERROR","CloudWatchLogsRoleArn":"arn:aws:iam::account:role/logs"}',
		}),
		field("XrayEnabled", "Enable X-Ray", { kind: "boolean" }),
		field("OwnerContact", "Owner contact"),
		field("IntrospectionConfig", "Introspection", {
			choices: ["ENABLED", "DISABLED"],
		}),
		field("QueryDepthLimit", "Query depth limit (0–75)", { kind: "number" }),
		field("ResolverCountLimit", "Resolver count limit (0–10000)", {
			kind: "number",
		}),
	],
	source: [
		field("Name", "Name", { required: true }),
		field("Type", "Data source type", { choices: sourceTypes, required: true }),
		field("Description", "Description"),
		field("ServiceRoleArn", "Service role ARN"),
		field("LambdaConfig", "Lambda configuration (JSON)", {
			kind: "json",
			schema: "LambdaDataSourceConfig",
			example:
				'{"LambdaFunctionArn":"arn:aws:lambda:region:account:function:name"}',
		}),
		field("DynamodbConfig", "DynamoDB configuration (JSON)", {
			kind: "json",
			schema: "DynamodbDataSourceConfig",
			example:
				'{"TableName":"table","AwsRegion":"us-east-1","UseCallerCredentials":false}',
		}),
		field("HttpConfig", "HTTP configuration (JSON)", {
			kind: "json",
			schema: "HttpDataSourceConfig",
			example: '{"Endpoint":"http://localhost:8080"}',
		}),
		field("EventBridgeConfig", "EventBridge configuration (JSON)", {
			kind: "json",
			schema: "EventBridgeDataSourceConfig",
			example:
				'{"EventBusArn":"arn:aws:events:region:account:event-bus/default"}',
		}),
	],
	resolver: [
		field("TypeName", "Type name", { required: true }),
		field("FieldName", "Field name", { required: true }),
		field("Kind", "Resolver kind", {
			choices: ["UNIT", "PIPELINE"],
			required: true,
		}),
		field("PipelineConfig", "Pipeline configuration (JSON)", {
			kind: "json",
			schema: "PipelineConfig",
			example: '{"Functions":["function-id"]}',
		}),
		...executionFields,
	],
	function: [
		field("Name", "Name", { required: true }),
		field("Description", "Description"),
		field("FunctionVersion", "Function version"),
		...executionFields,
	],
	key: [
		field("Description", "Description"),
		field("Expires", "Expiry (Unix seconds)", {
			kind: "number",
			required: true,
		}),
	],
};
const schemas: Record<string, string[]> = {
	LogConfig: [
		"FieldLogLevel",
		"CloudWatchLogsRoleArn",
		"ExcludeVerboseContent",
	],
	UserPoolConfig: [
		"UserPoolId",
		"AwsRegion",
		"DefaultAction",
		"AppIdClientRegex",
	],
	OpenIDConnectConfig: ["Issuer", "ClientId", "IatTTL", "AuthTTL"],
	AdditionalAuthenticationProvider: [
		"AuthenticationType",
		"OpenIDConnectConfig",
		"UserPoolConfig",
		"LambdaAuthorizerConfig",
	],
	CognitoUserPoolConfig: ["UserPoolId", "AwsRegion", "AppIdClientRegex"],
	LambdaAuthorizerConfig: [
		"AuthorizerResultTtlInSeconds",
		"AuthorizerUri",
		"IdentityValidationExpression",
	],
	DynamodbDataSourceConfig: ["TableName", "AwsRegion", "UseCallerCredentials"],
	LambdaDataSourceConfig: ["LambdaFunctionArn"],
	HttpDataSourceConfig: ["Endpoint", "AuthorizationConfig"],
	AuthorizationConfig: ["AuthorizationType", "AwsIamConfig"],
	AwsIamConfig: ["SigningRegion", "SigningServiceName"],
	EventBridgeDataSourceConfig: ["EventBusArn"],
	PipelineConfig: ["Functions"],
	AppSyncRuntime: ["Name", "RuntimeVersion"],
};
const nestedSchemas: Record<string, string> = {
	OpenIDConnectConfig: "OpenIDConnectConfig",
	UserPoolConfig: "CognitoUserPoolConfig",
	LambdaAuthorizerConfig: "LambdaAuthorizerConfig",
	AuthorizationConfig: "AuthorizationConfig",
	AwsIamConfig: "AwsIamConfig",
};
export function validateConfig(value: unknown, schema: string): void {
	if (schema === "AdditionalAuthenticationProvider") {
		if (!Array.isArray(value))
			throw new Error("Additional providers must be an array.");
		for (const item of value) validateConfigObject(item, schema);
	} else validateConfigObject(value, schema);
}
function validateConfigObject(value: unknown, schema: string) {
	if (!value || typeof value !== "object" || Array.isArray(value))
		throw new Error(`${schema} must be an object.`);
	for (const [key, child] of Object.entries(value)) {
		if (!schemas[schema]?.includes(key))
			throw new Error(`Unsupported ${schema} member: ${key}`);
		if (nestedSchemas[key]) validateConfigObject(child, nestedSchemas[key]);
	}
}
export function initialForm(
	kind: ResourceKind,
	row: RecordValue = {},
): Record<string, string> {
	const defaults: Record<string, unknown> = {
		AuthenticationType: "API_KEY",
		Type: "NONE",
		Kind: "UNIT",
		IntrospectionConfig: "ENABLED",
		XrayEnabled: false,
		FunctionVersion: "2018-05-29",
	};
	return Object.fromEntries(
		fields[kind].map((f) => {
			const v = row[f.key] ?? defaults[f.key];
			return [
				f.key,
				v === undefined
					? ""
					: f.kind === "json"
						? JSON.stringify(v, null, 2)
						: String(v),
			];
		}),
	);
}
export function formInput(
	kind: ResourceKind,
	form: Record<string, string>,
): RecordValue {
	const out: RecordValue = {};
	for (const f of fields[kind]) {
		const text = (form[f.key] ?? "").trim();
		if (!text) {
			if (f.required) throw new Error(`${f.label} is required.`);
			continue;
		}
		if (f.choices && !f.choices.includes(text))
			throw new Error(`Choose a valid ${f.label}.`);
		let value: unknown = text;
		if (f.kind === "number") {
			value = Number(text);
			if (!/^\d+$/.test(text) || !Number.isSafeInteger(value))
				throw new Error(`${f.label} must be a non-negative integer.`);
		}
		if (f.kind === "boolean") value = text === "true";
		if (f.kind === "json") {
			value = JSON.parse(text);
			if (f.schema) validateConfig(value, f.schema);
		}
		out[f.key] = value;
	}
	if (kind === "source") {
		const config: Record<string, string> = {
			AWS_LAMBDA: "LambdaConfig",
			AMAZON_DYNAMODB: "DynamodbConfig",
			HTTP: "HttpConfig",
			AMAZON_EVENTBRIDGE: "EventBridgeConfig",
		};
		for (const key of Object.values(config)) {
			if (key !== config[String(out.Type)] && out[key])
				throw new Error("Remove configuration for other data source types.");
		}
		if (config[String(out.Type)] && !out[config[String(out.Type)]])
			throw new Error(`Enter configuration for ${out.Type}.`);
	}
	if (kind === "resolver" || kind === "function") {
		if (Boolean(out.Runtime) !== Boolean(out.Code))
			throw new Error("Set runtime and JavaScript code together.");
		if (
			out.Runtime &&
			(out.RequestMappingTemplate || out.ResponseMappingTemplate)
		)
			throw new Error("Use JavaScript code or mapping templates.");
		if (out.MaxBatchSize !== undefined && Number(out.MaxBatchSize) > 2000)
			throw new Error("Maximum batch size is 2000.");
		if (out.Runtime) {
			const r = out.Runtime as RecordValue;
			if (r.Name !== "APPSYNC_JS" || r.RuntimeVersion !== "1.0.0")
				throw new Error("Runtime must be APPSYNC_JS 1.0.0.");
		}
	}
	return out;
}
export function resourceIdentity(
	kind: ResourceKind,
	row: RecordValue,
): RecordValue {
	if (kind === "api") return { ApiId: row.ApiId };
	if (kind === "resolver")
		return { TypeName: row.TypeName, FieldName: row.FieldName };
	if (kind === "function") return { FunctionId: row.FunctionId };
	if (kind === "key") return { Id: row.Id };
	return { Name: row.Name };
}
export function resourceLabel(kind: ResourceKind, row: RecordValue) {
	return String(
		kind === "resolver"
			? `${row.TypeName}.${row.FieldName}`
			: (row.Name ?? row.Id ?? row.FunctionId ?? row.ApiId),
	);
}
export function encodeSDL(sdl: string) {
	if (!sdl.trim()) throw new Error("Enter a schema in SDL format.");
	return btoa(
		Array.from(new TextEncoder().encode(sdl), (b) =>
			String.fromCharCode(b),
		).join(""),
	);
}
export function decodeSDL(base64: string) {
	return new TextDecoder().decode(
		Uint8Array.from(atob(base64), (c) => c.charCodeAt(0)),
	);
}
export function appsyncHref(
	scope: { account: string; region: string },
	apiId = "",
) {
	const params = new URLSearchParams(scope);
	if (apiId) params.set("appsyncApi", apiId);
	return `/services/appsync?${params}`;
}
