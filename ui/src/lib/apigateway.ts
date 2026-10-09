import { api } from "./api";
import { type ActionStep, stringMap } from "./consoleActions";
export type GatewayKind = "apigateway" | "apigatewayv2";
export type GatewayScope = {
	account: string;
	region: string;
	kind: GatewayKind;
	apiId?: string;
	resourceId?: string;
	method?: string;
};
export type GatewayAPI = {
	Id?: string;
	ApiId?: string;
	Name?: string;
	Description?: string;
	ApiEndpoint?: string;
	Tags?: Record<string, string>;
	DisableExecuteApiEndpoint?: boolean;
	CorsConfiguration?: Record<string, unknown>;
};
export type GatewayIntegration = {
	IntegrationId?: string;
	IntegrationType?: string;
	IntegrationUri?: string;
	IntegrationMethod?: string;
	PayloadFormatVersion?: string;
	Description?: string;
	TimeoutInMillis?: number;
	Type?: string;
	Uri?: string;
	HttpMethod?: string;
	[key: string]: unknown;
};
export type GatewayRoute = {
	RouteId: string;
	RouteKey: string;
	Target?: string;
	AuthorizationType?: string;
	AuthorizerId?: string;
	AuthorizationScopes?: string[];
	OperationName?: string;
};
export type GatewayMethod = {
	HttpMethod?: string;
	AuthorizationType?: string;
	AuthorizerId?: string;
	ApiKeyRequired?: boolean;
	AuthorizationScopes?: string[];
	OperationName?: string;
	MethodIntegration?: GatewayIntegration;
	[key: string]: unknown;
};
export type GatewayResource = {
	Id: string;
	ParentId?: string;
	Path: string;
	PathPart?: string;
	ResourceMethods?: Record<string, GatewayMethod>;
};
export type GatewayStage = {
	StageName: string;
	DeploymentId?: string;
	AutoDeploy?: boolean;
	Description?: string;
	StageVariables?: Record<string, string>;
	Variables?: Record<string, string>;
	AccessLogSettings?: Record<string, unknown>;
	Tags?: Record<string, string>;
	[key: string]: unknown;
};
export type GatewayDeployment = {
	Id?: string;
	DeploymentId?: string;
	Description?: string;
	CreatedDate?: string | number;
};
export type GatewayAuthorizer = {
	Id?: string;
	AuthorizerId?: string;
	Name?: string;
	Type?: string;
	AuthorizerType?: string;
	[key: string]: unknown;
};
export const isHTTP = (scope: GatewayScope) => scope.kind === "apigatewayv2";
export function gatewayHref(scope: GatewayScope, section = "apis") {
	const p = new URLSearchParams({
		account: scope.account,
		region: scope.region,
		section,
	});
	if (scope.apiId) p.set("apiId", scope.apiId);
	if (scope.resourceId) p.set("resourceId", scope.resourceId);
	if (scope.method) p.set("method", scope.method);
	return `/services/${scope.kind}?${p}`;
}
export function gatewayCall<T = Record<string, unknown>>(
	scope: GatewayScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action(scope.kind, operation, scope.account, input, scope.region);
}
export function apiCall<T = Record<string, unknown>>(
	scope: GatewayScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return gatewayCall(scope, operation, {
		[isHTTP(scope) ? "ApiId" : "RestApiId"]: scope.apiId,
		...input,
	});
}
export async function gatewayList<T>(
	scope: GatewayScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T[]> {
	const http = isHTTP(scope),
		out: T[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const page = await apiCall<{
			Items?: T[];
			Item?: T[];
			NextToken?: string;
			Position?: string;
		}>(scope, operation, {
			...input,
			...(http ? { MaxResults: "100" } : { Limit: 100 }),
			...(token ? { [http ? "NextToken" : "Position"]: token } : {}),
		});
		out.push(...(page.Items ?? page.Item ?? []));
		token = http ? page.NextToken : page.Position;
		if (token && seen.has(token))
			throw new Error(
				"API Gateway pagination did not advance. Refresh to retry.",
			);
		if (token) seen.add(token);
	} while (token);
	return out;
}
export async function gatewaySnapshot(scope: GatewayScope) {
	const http = isHTTP(scope);
	const api = await apiCall<GatewayAPI>(scope, http ? "GetApi" : "GetRestApi");
	const [routes, resources, integrations, stages, deployments, authorizers] =
		await Promise.all([
			http
				? gatewayList<GatewayRoute>(scope, "GetRoutes")
				: Promise.resolve([]),
			http
				? Promise.resolve([])
				: gatewayList<GatewayResource>(scope, "GetResources", {
						Embed: ["methods"],
					}),
			http
				? gatewayList<GatewayIntegration>(scope, "GetIntegrations")
				: Promise.resolve([]),
			// REST GetStages has no pagination fields.
			http
				? gatewayList<GatewayStage>(scope, "GetStages")
				: apiCall<{ Item?: GatewayStage[] }>(scope, "GetStages").then(
						(p) => p.Item ?? [],
					),
			gatewayList<GatewayDeployment>(scope, "GetDeployments"),
			gatewayList<GatewayAuthorizer>(scope, "GetAuthorizers"),
		]);
	return {
		api,
		routes,
		resources: resources.sort((a, b) => a.Path.localeCompare(b.Path)),
		integrations,
		stages,
		deployments,
		authorizers,
	};
}
export type GatewaySnapshot = Awaited<ReturnType<typeof gatewaySnapshot>>;
export function gatewayARN(scope: GatewayScope, stage?: string) {
	return `arn:aws:apigateway:${scope.region}::/${isHTTP(scope) ? "apis" : "restapis"}/${scope.apiId}${stage ? `/stages/${stage}` : ""}`;
}
export function gatewayTagPlan(
	scope: GatewayScope,
	arn: string,
	tags: Record<string, string>,
	removed: string[],
): ActionStep[] {
	return [
		...(removed.length
			? [
					{
						label: "Remove tags",
						run: () =>
							gatewayCall(scope, "UntagResource", {
								ResourceArn: arn,
								TagKeys: removed,
							}),
					},
				]
			: []),
		...(Object.keys(tags).length
			? [
					{
						label: "Save tags",
						run: () =>
							gatewayCall(scope, "TagResource", {
								ResourceArn: arn,
								Tags: tags,
							}),
					},
				]
			: []),
	];
}
export function stringLines(text: string) {
	return text
		.split("\n")
		.map((s) => s.trim())
		.filter(Boolean);
}
export function mapPatch(
	field: string,
	next: Record<string, unknown>,
	old: Record<string, unknown> = {},
) {
	const path = (key: string) =>
		`/${field}/${key.replace(/~/g, "~0").replace(/\//g, "~1")}`;
	return [
		...Object.keys(old)
			.filter((k) => !(k in next))
			.map((k) => ({ Op: "remove", Path: path(k) })),
		...Object.entries(next).map(([k, v]) => ({
			Op: "replace",
			Path: path(k),
			Value: String(v),
		})),
	];
}
export const scalarPatch = (values: Record<string, unknown>) =>
	Object.entries(values).map(([key, value]) => ({
		Op: "replace",
		Path: `/${key}`,
		Value: String(value),
	}));
export function stageInput(
	scope: GatewayScope,
	stage: GatewayStage,
	next: {
		description: string;
		auto: boolean;
		deployment: string;
		variables: string;
		logs: string;
	},
) {
	const variables = stringMap(next.variables, "Stage variables"),
		logs = stringMap(next.logs, "Access log settings");
	if (Object.keys(logs).length && (!logs.DestinationArn || !logs.Format))
		throw new Error("Access logs need DestinationArn and Format.");
	if (Object.keys(logs).some((k) => k !== "DestinationArn" && k !== "Format"))
		throw new Error("Access log settings support DestinationArn and Format.");
	return isHTTP(scope)
		? {
				StageName: stage.StageName,
				Description: next.description,
				AutoDeploy: next.auto,
				StageVariables: variables,
				AccessLogSettings: logs,
				...(!next.auto && next.deployment
					? { DeploymentId: next.deployment }
					: {}),
			}
		: {
				StageName: stage.StageName,
				PatchOperations: [
					...scalarPatch({
						description: next.description,
						...(next.deployment ? { deploymentId: next.deployment } : {}),
					}),
					...mapPatch("variables", variables, stage.Variables),
					...(Object.keys(logs).length
						? scalarPatch({
								"accessLogSettings/destinationArn": logs.DestinationArn,
								"accessLogSettings/format": logs.Format,
							})
						: [{ Op: "remove", Path: "/accessLogSettings" }]),
				],
			};
}
export function validateURI(type: string, uri: string) {
	if (type === "AWS_PROXY" && !/^arn:aws[a-z-]*:(lambda|apigateway):/.test(uri))
		throw new Error("Choose a Lambda function ARN or invocation URI.");
	if (type === "HTTP_PROXY" && !/^https?:\/\/\S+/.test(uri))
		throw new Error("Enter an HTTP or HTTPS integration URL.");
}
export function listPatch(field: string, next: string[], old: string[] = []) {
	return [
		...old.map((Value) => ({ Op: "remove", Path: `/${field}`, Value })),
		...next.map((Value) => ({ Op: "add", Path: `/${field}`, Value })),
	];
}
