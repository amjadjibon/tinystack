import {
	Alert,
	Box,
	Button,
	Checkbox,
	Container,
	FormField,
	Header,
	Link,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import {
	apiCall,
	type GatewayMethod,
	type GatewayResource,
	type GatewayScope,
	type GatewaySnapshot,
	gatewayHref,
	listPatch,
	scalarPatch,
	stringLines,
	validateURI,
} from "#/lib/apigateway";
import { integer } from "#/lib/consoleActions";
import { useScope } from "#/lib/scope";
import { useFollow } from "#/lib/useFollow";
import { GatewayInput, GatewaySelect, GatewayText, options } from "./Fields";
import { GatewayTable } from "./GatewayTable";
export function RestResources({
	scope,
	snapshot,
	enabled,
	onChanged,
	onNavigate,
}: {
	scope: GatewayScope;
	snapshot: GatewaySnapshot;
	enabled: boolean;
	onChanged: (message: string) => void;
	onNavigate: (resourceId: string, method?: string) => void;
}) {
	const follow = useFollow(),
		[dialog, setDialog] = useState(""),
		[parent, setParent] = useState(
			scope.resourceId ??
				snapshot.resources.find((r) => r.Path === "/")?.Id ??
				"",
		),
		[part, setPart] = useState("");
	const resource = snapshot.resources.find((r) => r.Id === scope.resourceId),
		method = resource?.ResourceMethods?.[scope.method ?? ""];
	const saved = (message: string) => {
		setDialog("");
		onChanged(message);
	};
	return (
		<SpaceBetween size="l">
			<Alert type="info">
				Resource and method changes require deployment before they affect
				requests.
			</Alert>
			<GatewayTable
				items={snapshot.resources}
				trackBy="Id"
				header={
					<Header
						actions={
							<Button
								variant="primary"
								disabled={!enabled}
								onClick={() => setDialog("resource")}
							>
								Create resource
							</Button>
						}
					>
						Resources
					</Header>
				}
				wrapLines
				columnDefinitions={[
					{
						id: "path",
						header: "Path",
						cell: (r) => (
							<Link
								href={gatewayHref(
									{ ...scope, resourceId: r.Id, method: "" },
									"routes",
								)}
								onFollow={follow}
							>
								{r.Path}
							</Link>
						),
					},
					{
						id: "methods",
						header: "Methods",
						cell: (r) =>
							Object.keys(r.ResourceMethods ?? {}).join(", ") || "None",
					},
					{ id: "id", header: "ID", cell: (r) => r.Id },
				]}
				empty={<Box>No resources.</Box>}
			/>
			{!!scope.resourceId && !resource && (
				<Alert type="error">
					Resource not found. Choose a resource from the list.
				</Alert>
			)}
			{resource && (
				<Container
					header={
						<Header
							actions={
								<SpaceBetween direction="horizontal" size="xs">
									<Button
										disabled={
											!enabled ||
											Object.keys(resource.ResourceMethods ?? {}).length >= 8
										}
										onClick={() => setDialog("method")}
									>
										Create method
									</Button>
									<Button
										disabled={!enabled || resource.Path === "/"}
										onClick={() => setDialog("delete-resource")}
									>
										Delete resource
									</Button>
								</SpaceBetween>
							}
						>
							{resource.Path}
						</Header>
					}
				>
					<SpaceBetween size="l">
						<GatewayTable
							items={Object.entries(resource.ResourceMethods ?? {}).map(
								([name, m]) => ({ ...m, name }),
							)}
							trackBy="name"
							variant="embedded"
							columnDefinitions={[
								{
									id: "method",
									header: "Method",
									cell: (m) => (
										<Link
											href={gatewayHref({ ...scope, method: m.name }, "routes")}
											onFollow={follow}
										>
											{m.name}
										</Link>
									),
								},
								{
									id: "auth",
									header: "Authorization",
									cell: (m) => m.AuthorizationType ?? "NONE",
								},
								{
									id: "integration",
									header: "Integration",
									cell: (m) => m.MethodIntegration?.Type ?? "None",
								},
							]}
							empty={<Box>No methods. Create a method and integration.</Box>}
						/>
						{!!scope.method && !method && (
							<Alert type="error">Method not found.</Alert>
						)}
						{method && (
							<Container
								header={
									<Header
										actions={
											<SpaceBetween direction="horizontal" size="xs">
												<Button
													disabled={!enabled}
													onClick={() => setDialog("edit-method")}
												>
													Edit method and integration
												</Button>
												<Button
													disabled={!enabled}
													onClick={() => setDialog("delete-method")}
												>
													Delete method
												</Button>
											</SpaceBetween>
										}
									>
										{scope.method}
									</Header>
								}
							>
								<CodeBlock text={JSON.stringify(method, null, 2)} />
							</Container>
						)}
					</SpaceBetween>
				</Container>
			)}
			{(dialog === "method" || dialog === "edit-method") && resource && (
				<RestMethodForm
					scope={scope}
					resource={resource}
					snapshot={snapshot}
					initial={dialog === "edit-method" ? method : undefined}
					onClose={() => setDialog("")}
					onSaved={(name) => {
						saved("Method and integration saved.");
						onNavigate(resource.Id, name);
					}}
				/>
			)}
			{dialog === "resource" && (
				<ActionDialog
					title="Create resource"
					submit="Create resource"
					onClose={() => setDialog("")}
					onSaved={() => saved("Resource created.")}
					plan={async () => {
						if (!parent || !part.trim())
							throw new Error("Choose a parent and enter a path part.");
						return [
							{
								label: "Create resource",
								run: () =>
									apiCall(scope, "CreateResource", {
										ParentId: parent,
										PathPart: part,
									}),
							},
						];
					}}
				>
					<SpaceBetween size="l">
						<GatewaySelect
							label="Parent resource"
							value={parent}
							onChange={setParent}
							options={snapshot.resources.map((r) => ({
								value: r.Id,
								label: r.Path,
							}))}
						/>
						<GatewayInput
							label="Path part"
							value={part}
							onChange={setPart}
							description="One segment, such as pets, {id}, or {proxy+}."
						/>
					</SpaceBetween>
				</ActionDialog>
			)}
			{(dialog === "delete-method" || dialog === "delete-resource") &&
				resource && (
					<ActionDialog
						title={
							dialog === "delete-method" ? "Delete method" : "Delete resource"
						}
						submit="Delete"
						confirmation={
							dialog === "delete-method" ? scope.method : resource.Path
						}
						onClose={() => setDialog("")}
						onSaved={() => {
							saved("Deleted.");
							onNavigate(dialog === "delete-resource" ? "" : resource.Id);
						}}
						plan={async () => [
							{
								label: "Delete",
								run: () =>
									apiCall(
										scope,
										dialog === "delete-method"
											? "DeleteMethod"
											: "DeleteResource",
										{
											ResourceId: resource.Id,
											...(dialog === "delete-method"
												? { HttpMethod: scope.method }
												: {}),
										},
									),
							},
						]}
					>
						<Alert type="warning">
							{dialog === "delete-resource"
								? "This removes the resource, its children and their methods."
								: "This removes the method and its integration."}{" "}
							Existing deployments continue serving their snapshots until
							redeployed.
						</Alert>
					</ActionDialog>
				)}
		</SpaceBetween>
	);
}
function RestMethodForm({
	scope,
	resource,
	snapshot,
	initial,
	onClose,
	onSaved,
}: {
	scope: GatewayScope;
	resource: GatewayResource;
	snapshot: GatewaySnapshot;
	initial?: GatewayMethod;
	onClose: () => void;
	onSaved: (name: string) => void;
}) {
	const { inventory } = useScope();
	const [method, setMethod] = useState(
			initial?.HttpMethod ??
				(initial
					? scope.method
					: Object.keys({
							GET: 0,
							POST: 0,
							PUT: 0,
							PATCH: 0,
							DELETE: 0,
							HEAD: 0,
							OPTIONS: 0,
							ANY: 0,
						}).find((m) => !resource.ResourceMethods?.[m])) ??
				"GET",
		),
		[auth, setAuth] = useState(initial?.AuthorizationType ?? "NONE"),
		[authorizer, setAuthorizer] = useState(initial?.AuthorizerId ?? ""),
		[apiKey, setApiKey] = useState(initial?.ApiKeyRequired ?? false),
		[scopes, setScopes] = useState(
			(initial?.AuthorizationScopes ?? []).join("\n"),
		);
	const integration = initial?.MethodIntegration,
		[type, setType] = useState(integration?.Type ?? "AWS_PROXY"),
		[uri, setURI] = useState(integration?.Uri ?? ""),
		[verb, setVerb] = useState(integration?.HttpMethod ?? "GET"),
		[timeout, setTimeout] = useState(
			String(integration?.TimeoutInMillis ?? 29000),
		);
	const functions = (inventory.data ?? [])
		.filter(
			(r) =>
				r.service === "lambda" &&
				r.type === "function" &&
				r.account === scope.account &&
				r.region === scope.region,
		)
		.map((r) => ({ value: r.arn, label: r.name }));
	const authorizers = snapshot.authorizers
		.filter(
			(a) =>
				(a.Type === "COGNITO_USER_POOLS") === (auth === "COGNITO_USER_POOLS"),
		)
		.map((a) => ({ value: a.Id ?? "", label: a.Name ?? a.Id ?? "" }));
	return (
		<ActionDialog
			title={initial ? "Edit method and integration" : "Create method"}
			submit={initial ? "Save changes" : "Create method"}
			onClose={onClose}
			onSaved={() => onSaved(method)}
			plan={async () => {
				if (type !== "MOCK") validateURI(type, uri);
				if ((auth === "CUSTOM" || auth === "COGNITO_USER_POOLS") && !authorizer)
					throw new Error("Choose an authorizer.");
				const scopeList =
						auth === "COGNITO_USER_POOLS" ? stringLines(scopes) : [],
					input = { ResourceId: resource.Id, HttpMethod: method };
				const integrationInput = {
					Type: type,
					...(type !== "MOCK" ? { Uri: uri } : {}),
					IntegrationHttpMethod: type === "AWS_PROXY" ? "POST" : verb,
					TimeoutInMillis: integer(timeout, "Timeout", 50, 29000),
				};
				return [
					{
						label: "Save method",
						run: () =>
							apiCall(scope, initial ? "UpdateMethod" : "PutMethod", {
								...input,
								...(initial
									? {
											PatchOperations: [
												...scalarPatch({
													authorizationType: auth,
													apiKeyRequired: apiKey,
													...(authorizer &&
													auth !== "NONE" &&
													auth !== "AWS_IAM"
														? { authorizerId: authorizer }
														: {}),
												}),
												...listPatch(
													"authorizationScopes",
													scopeList,
													initial.AuthorizationScopes,
												),
											],
										}
									: {
											AuthorizationType: auth,
											ApiKeyRequired: apiKey,
											...(authorizer && auth !== "NONE" && auth !== "AWS_IAM"
												? { AuthorizerId: authorizer }
												: {}),
											AuthorizationScopes: scopeList,
										}),
							}),
					},
					{
						label: "Save integration",
						run: () =>
							apiCall(
								scope,
								integration ? "UpdateIntegration" : "PutIntegration",
								{
									...input,
									...(integration
										? {
												PatchOperations: scalarPatch({
													type,
													...(type !== "MOCK" ? { uri } : {}),
													httpMethod: integrationInput.IntegrationHttpMethod,
													timeoutInMillis: integrationInput.TimeoutInMillis,
												}),
											}
										: integrationInput),
								},
							),
					},
				];
			}}
		>
			<SpaceBetween size="l">
				<GatewaySelect
					label="HTTP method"
					value={method}
					onChange={setMethod}
					options={options(
						initial
							? [method]
							: [
									"GET",
									"POST",
									"PUT",
									"PATCH",
									"DELETE",
									"HEAD",
									"OPTIONS",
									"ANY",
								].filter((m) => !resource.ResourceMethods?.[m]),
					)}
				/>
				<GatewaySelect
					label="Authorization"
					value={auth}
					onChange={(v) => {
						setAuth(v);
						setAuthorizer("");
					}}
					options={options(["NONE", "AWS_IAM", "CUSTOM", "COGNITO_USER_POOLS"])}
				/>
				{(auth === "CUSTOM" || auth === "COGNITO_USER_POOLS") && (
					<GatewaySelect
						label="Authorizer"
						value={authorizer}
						onChange={setAuthorizer}
						options={authorizers}
					/>
				)}
				{auth === "COGNITO_USER_POOLS" && (
					<GatewayText
						label="Authorization scopes"
						value={scopes}
						onChange={setScopes}
						description="One scope per line."
					/>
				)}
				<FormField label="API keys">
					<Checkbox
						checked={apiKey}
						onChange={({ detail }) => setApiKey(detail.checked)}
					>
						Require an API key
					</Checkbox>
				</FormField>
				<GatewaySelect
					label="Integration type"
					value={type}
					onChange={setType}
					options={options(["AWS_PROXY", "HTTP_PROXY", "MOCK"])}
				/>
				{type === "AWS_PROXY" && (
					<GatewaySelect
						label="Existing Lambda function"
						value={uri}
						onChange={setURI}
						options={functions}
					/>
				)}
				{type !== "MOCK" && (
					<GatewayInput
						label={
							type === "AWS_PROXY"
								? "Lambda function ARN or invocation URI"
								: "HTTP endpoint URL"
						}
						value={uri}
						onChange={setURI}
					/>
				)}
				{type === "HTTP_PROXY" && (
					<GatewaySelect
						label="Integration HTTP method"
						value={verb}
						onChange={setVerb}
						options={options([
							"ANY",
							"GET",
							"POST",
							"PUT",
							"PATCH",
							"DELETE",
							"HEAD",
							"OPTIONS",
						])}
					/>
				)}
				<GatewayInput
					label="Timeout (milliseconds)"
					value={timeout}
					onChange={setTimeout}
				/>
				<Alert type="info">
					Deploy this API to publish method changes. API key and usage plan
					management uses the CLI/SDK.
				</Alert>
			</SpaceBetween>
		</ActionDialog>
	);
}
