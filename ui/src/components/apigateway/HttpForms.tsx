import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import {
	apiCall,
	type GatewayIntegration,
	type GatewayRoute,
	type GatewayScope,
	type GatewaySnapshot,
	stringLines,
	validateURI,
} from "#/lib/apigateway";
import { integer } from "#/lib/consoleActions";
import { useScope } from "#/lib/scope";
import { GatewayInput, GatewaySelect, GatewayText, options } from "./Fields";
export function HttpRouteForm({
	scope,
	snapshot,
	initial,
	onClose,
	onSaved,
}: {
	scope: GatewayScope;
	snapshot: GatewaySnapshot;
	initial?: GatewayRoute;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [key, setKey] = useState(initial?.RouteKey ?? "GET /"),
		[target, setTarget] = useState(initial?.Target ?? ""),
		[auth, setAuth] = useState(initial?.AuthorizationType ?? "NONE");
	const [authorizer, setAuthorizer] = useState(initial?.AuthorizerId ?? ""),
		[scopes, setScopes] = useState(
			(initial?.AuthorizationScopes ?? []).join("\n"),
		);
	const authorizers = snapshot.authorizers
		.filter((a) => a.AuthorizerType === (auth === "JWT" ? "JWT" : "REQUEST"))
		.map((a) => ({
			value: a.AuthorizerId ?? "",
			label: a.Name ?? a.AuthorizerId ?? "",
		}));
	return (
		<ActionDialog
			title={initial ? "Edit route" : "Create route"}
			submit={initial ? "Save changes" : "Create route"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				if (
					key !== "$default" &&
					!/^(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS|ANY) \/[^\s?#]*$/.test(key)
				)
					throw new Error(
						"Use $default or a method and path, such as GET /pets/{id}.",
					);
				if ((auth === "JWT" || auth === "CUSTOM") && !authorizer)
					throw new Error("Choose an authorizer for this route.");
				return [
					{
						label: "Save route",
						run: () =>
							apiCall(scope, initial ? "UpdateRoute" : "CreateRoute", {
								...(initial ? { RouteId: initial.RouteId } : {}),
								RouteKey: key,
								Target: target,
								AuthorizationType: auth,
								AuthorizerId:
									auth === "JWT" || auth === "CUSTOM" ? authorizer : "",
								AuthorizationScopes: auth === "JWT" ? stringLines(scopes) : [],
							}),
					},
				];
			}}
		>
			<SpaceBetween size="l">
				<GatewayInput
					label="Route key"
					value={key}
					onChange={setKey}
					description="GET /pets/{id}, ANY /{proxy+}, or $default"
				/>
				<GatewaySelect
					label="Integration"
					value={target}
					onChange={setTarget}
					options={[
						{ value: "", label: "No integration" },
						...snapshot.integrations.map((i) => ({
							value: `integrations/${i.IntegrationId}`,
							label: `${i.IntegrationId} · ${i.IntegrationUri}`,
						})),
					]}
				/>
				<GatewaySelect
					label="Authorization"
					value={auth}
					onChange={(v) => {
						setAuth(v);
						setAuthorizer("");
					}}
					options={options(["NONE", "AWS_IAM", "JWT", "CUSTOM"])}
				/>
				{(auth === "JWT" || auth === "CUSTOM") && (
					<GatewaySelect
						label="Authorizer"
						value={authorizer}
						onChange={setAuthorizer}
						options={authorizers}
					/>
				)}
				{auth === "JWT" && (
					<GatewayText
						label="Authorization scopes"
						description="One required scope per line."
						value={scopes}
						onChange={setScopes}
					/>
				)}
				<Alert type="info">
					Changes are applied immediately to stages with auto deploy. Other
					stages require a new deployment.
				</Alert>
			</SpaceBetween>
		</ActionDialog>
	);
}
export function IntegrationForm({
	scope,
	initial,
	onClose,
	onSaved,
}: {
	scope: GatewayScope;
	initial?: GatewayIntegration;
	onClose: () => void;
	onSaved: () => void;
}) {
	const { inventory } = useScope();
	const [type, setType] = useState(initial?.IntegrationType ?? "AWS_PROXY"),
		[uri, setURI] = useState(initial?.IntegrationUri ?? ""),
		[payload, setPayload] = useState(initial?.PayloadFormatVersion ?? "2.0"),
		[method, setMethod] = useState(initial?.IntegrationMethod ?? "POST"),
		[description, setDescription] = useState(initial?.Description ?? ""),
		[timeout, setTimeout] = useState(String(initial?.TimeoutInMillis ?? 30000));
	const functions = (inventory.data ?? [])
		.filter(
			(r) =>
				r.service === "lambda" &&
				r.type === "function" &&
				r.account === scope.account &&
				r.region === scope.region,
		)
		.map((r) => ({ value: r.arn, label: r.name }));
	return (
		<ActionDialog
			title={initial ? "Edit integration" : "Create integration"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				validateURI(type, uri);
				return [
					{
						label: "Save integration",
						run: () =>
							apiCall(
								scope,
								initial ? "UpdateIntegration" : "CreateIntegration",
								{
									...(initial ? { IntegrationId: initial.IntegrationId } : {}),
									IntegrationType: type,
									IntegrationUri: uri,
									IntegrationMethod: type === "AWS_PROXY" ? "POST" : method,
									PayloadFormatVersion: type === "HTTP_PROXY" ? "1.0" : payload,
									Description: description,
									TimeoutInMillis: integer(timeout, "Timeout", 50, 30000),
								},
							),
					},
				];
			}}
		>
			<SpaceBetween size="l">
				<GatewaySelect
					label="Integration type"
					value={type}
					onChange={(v) => {
						setType(v);
						setPayload(v === "HTTP_PROXY" ? "1.0" : "2.0");
					}}
					options={options(["AWS_PROXY", "HTTP_PROXY"])}
				/>
				{type === "AWS_PROXY" && (
					<GatewaySelect
						label="Existing Lambda function"
						value={uri}
						onChange={setURI}
						options={functions}
					/>
				)}
				<GatewayInput
					label={
						type === "AWS_PROXY"
							? "Lambda function ARN or invocation URI"
							: "HTTP endpoint URL"
					}
					value={uri}
					onChange={setURI}
				/>
				{type === "AWS_PROXY" ? (
					<GatewaySelect
						label="Payload format version"
						value={payload}
						onChange={setPayload}
						options={options(["1.0", "2.0"])}
					/>
				) : (
					<GatewaySelect
						label="Integration HTTP method"
						value={method}
						onChange={setMethod}
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
				<GatewayInput
					label="Description"
					value={description}
					onChange={setDescription}
				/>
				<Alert type="info">
					Attach this integration to one or more routes. Changes affect
					auto-deployed stages immediately.
				</Alert>
			</SpaceBetween>
		</ActionDialog>
	);
}
